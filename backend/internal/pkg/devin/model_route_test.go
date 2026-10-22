package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func TestModelRouteClassification(t *testing.T) {
	for _, tc := range []struct {
		name             string
		model            *pb.ClientModelConfig
		kind, mode, code string
	}{
		{"ordinary", &pb.ClientModelConfig{ModelUid: "swe-2-max"}, KindDirect, RouteDirect, ""},
		{"unknown ordinary", &pb.ClientModelConfig{ModelUid: "future-model-with-many-hyphens"}, KindDirect, RouteDirect, ""},
		{"router flag", &pb.ClientModelConfig{ModelUid: "router", ModelInfo: &pb.ModelInfo{IsModelRouter: true}}, KindRouter, RouteAssign, ""},
		{"router display", &pb.ClientModelConfig{ModelUid: "router", ModelInfo: &pb.ModelInfo{DisplayOption: pb.DisplayOption_DISPLAY_OPTION_MODEL_ROUTER}}, KindRouter, RouteAssign, ""},
		{"opaque harness", &pb.ClientModelConfig{ModelUid: "opaque", ModelInfo: &pb.ModelInfo{HarnessUids: []string{"fusion"}}}, KindHarness, RouteBlocked, CodeRouteUnverified},
		{"harness router", &pb.ClientModelConfig{ModelUid: "opaque", ModelInfo: &pb.ModelInfo{IsModelRouter: true, HarnessUids: []string{"other"}}}, KindHarness, RouteBlocked, CodeRouteUnverified},
		{"pair", &pb.ClientModelConfig{ModelUid: "fusion-lead-sidekick-worker"}, KindPairing, RouteBlocked, CodeFusionUnsupported},
		{"fast pair", &pb.ClientModelConfig{ModelUid: "fusion-lead-fast-priority-sidekick-many-hyphen-worker"}, KindPairing, RouteBlocked, CodeFusionUnsupported},
		{"bare", &pb.ClientModelConfig{ModelUid: "fusion"}, KindUnknownSpecial, RouteBlocked, CodeRouteUnverified},
		{"sidekick", &pb.ClientModelConfig{ModelUid: "fusion-sidekick-worker"}, KindUnknownSpecial, RouteBlocked, CodeRouteUnverified},
		{"future fusion format", &pb.ClientModelConfig{ModelUid: "fusion_v2_fast"}, KindUnknownSpecial, RouteBlocked, CodeRouteUnverified},
		{"opaque fusion label", &pb.ClientModelConfig{ModelUid: "opaque", Label: "Fusion Fast", ModelInfo: &pb.ModelInfo{IsModelRouter: true}}, KindUnknownSpecial, RouteBlocked, CodeRouteUnverified},
		{"empty lead", &pb.ClientModelConfig{ModelUid: "fusion--sidekick-worker"}, KindUnknownSpecial, RouteBlocked, CodeRouteUnverified},
		{"empty worker", &pb.ClientModelConfig{ModelUid: "fusion-lead-sidekick-"}, KindUnknownSpecial, RouteBlocked, CodeRouteUnverified},
		{"case", &pb.ClientModelConfig{ModelUid: "FUSION-LEAD-SIDEKICK-WORKER"}, KindPairing, RouteBlocked, CodeFusionUnsupported},
		{"alternate uid", &pb.ClientModelConfig{ModelUid: "opaque", ModelInfo: &pb.ModelInfo{ModelUid: "fusion"}}, KindUnknownSpecial, RouteBlocked, CodeRouteUnverified},
		{"model info uid", &pb.ClientModelConfig{ModelInfo: &pb.ModelInfo{ModelUid: "swe"}}, KindDirect, RouteDirect, ""},
		{"disabled", &pb.ClientModelConfig{ModelUid: "swe", Disabled: true}, KindDirect, RouteBlocked, CodeModelUnavailable},
		{"nil", nil, KindDirect, RouteBlocked, CodeModelUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := PlanModel(tc.model)
			if p.CatalogKind != tc.kind || p.Mode != tc.mode || p.DecisionCode != tc.code {
				t.Fatalf("plan: %+v", p)
			}
			if (p.Validate() != nil) != (tc.mode == RouteBlocked) {
				t.Fatalf("validation: %+v", p)
			}
			if strings.Contains(p.MechanismStatus, "verified") && p.MechanismStatus != "unverified" {
				t.Fatal("classification claimed execution evidence")
			}
		})
	}
}

func TestModelInfoIndependentSchemaFixture(t *testing.T) {
	descriptor, err := os.ReadFile("testdata/model_routing/model_info.descriptor.pb")
	if err != nil {
		t.Fatal(err)
	}
	var set descriptorpb.FileDescriptorSet
	if err = proto.Unmarshal(descriptor, &set); err != nil {
		t.Fatal(err)
	}
	files, err := protodesc.NewFiles(&set)
	if err != nil {
		t.Fatal(err)
	}
	d, err := files.FindDescriptorByName("exa.codeium_common_pb.ModelInfo")
	if err != nil {
		t.Fatal(err)
	}
	message, ok := d.(protoreflect.MessageDescriptor)
	if !ok {
		t.Fatal("fixture descriptor is not a message")
	}
	reference := dynamicpb.NewMessage(message)
	field := reference.Descriptor().Fields().ByName("harness_uids")
	local := (&pb.ModelInfo{}).ProtoReflect().Descriptor().Fields().ByName("harness_uids")
	if field.Number() != 20 || field.Kind() != protoreflect.StringKind || field.Cardinality() != protoreflect.Repeated || local.Number() != field.Number() || local.Kind() != field.Kind() || local.Cardinality() != field.Cardinality() {
		t.Fatal("application differs from the independent upstream declaration")
	}
	payload, err := os.ReadFile("testdata/model_routing/model_info.bin")
	if err != nil {
		t.Fatal(err)
	}
	if err = proto.Unmarshal(payload, reference); err != nil {
		t.Fatal(err)
	}
	var info pb.ModelInfo
	if err = proto.Unmarshal(payload, &info); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(info.HarnessUids, []string{"fusion", "synthetic-secondary-harness"}) || info.ModelUid != "opaque-catalog-entry" || info.MaxOutputTokens != 4096 || !info.IsModelRouter {
		t.Fatalf("independent payload decoded incorrectly: %v", &info)
	}
}

func TestPrepareChecksBeforeAuthorizationAndAfterAssignment(t *testing.T) {
	for _, tc := range []struct {
		name       string
		selected   *pb.ClientModelConfig
		assignment *pb.ModelAssignment
		extra      []*pb.ClientModelConfig
		code       string
		rpcs       int
	}{
		{"pair preflight", &pb.ClientModelConfig{ModelUid: "fusion-lead-sidekick-worker"}, nil, nil, CodeFusionUnsupported, 0},
		{"harness preflight", &pb.ClientModelConfig{ModelUid: "harness", ModelInfo: &pb.ModelInfo{HarnessUids: []string{"custom"}}}, nil, nil, CodeRouteUnverified, 0},
		{"ordinary dynamic", nil, &pb.ModelAssignment{ModelUid: "dynamic-target", AssignmentJwt: "private-assignment"}, nil, "", 2},
		{"assignment harness", nil, &pb.ModelAssignment{ModelUid: "lead", AssignmentJwt: "private-assignment", HarnessUids: []string{"fusion"}}, nil, CodeAssignmentHarness, 2},
		{"assignment pair", nil, &pb.ModelAssignment{ModelUid: "fusion-lead-sidekick-worker", AssignmentJwt: "private-assignment"}, nil, CodeRouteMismatch, 2},
		{"assignment unknown", nil, &pb.ModelAssignment{ModelUid: "fusion", AssignmentJwt: "private-assignment"}, nil, CodeRouteMismatch, 2},
		{"assignment no jwt", nil, &pb.ModelAssignment{ModelUid: "lead"}, nil, CodeRouteMismatch, 2},
		{"assignment no uid", nil, &pb.ModelAssignment{AssignmentJwt: "private-assignment"}, nil, CodeRouteMismatch, 2},
		{"assignment catalog harness", nil, &pb.ModelAssignment{ModelUid: "hidden", AssignmentJwt: "private-assignment"}, []*pb.ClientModelConfig{{ModelUid: "hidden", ModelInfo: &pb.ModelInfo{HarnessUids: []string{"harness"}}}}, CodeRouteMismatch, 2},
		{"assignment another router", nil, &pb.ModelAssignment{ModelUid: "other-router", AssignmentJwt: "private-assignment"}, []*pb.ClientModelConfig{{ModelUid: "other-router", ModelInfo: &pb.ModelInfo{IsModelRouter: true}}}, CodeRouteMismatch, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := tc.selected
			if model == nil {
				model = &pb.ClientModelConfig{ModelUid: "router", ModelInfo: &pb.ModelInfo{DisplayOption: pb.DisplayOption_DISPLAY_OPTION_MODEL_ROUTER}}
			}
			catalog := append([]*pb.ClientModelConfig{model}, tc.extra...)
			calls := 0
			client := &Client{BaseURL: "https://example.test", Do: func(r *http.Request) (*http.Response, error) {
				calls++
				var payload proto.Message
				switch r.URL.Path {
				case UserJWTPath:
					payload = &pb.GetUserJwtResponse{UserJwt: "private-user-jwt"}
				case AssignPath:
					payload = &pb.AssignModelResponse{Assignment: tc.assignment}
				default:
					t.Fatalf("unexpected generation/control RPC: %s", r.URL.Path)
				}
				b, err := proto.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b))}, nil
			}}
			q := &pb.GetChatMessageRequest{ChatModelUid: ModelID(model)}
			prepared, err := client.Prepare(context.Background(), "private-token", q, PlanModel(model), catalog)
			var routeErr *RouteError
			if tc.code == "" {
				if err != nil || q.ChatModelUid != tc.assignment.ModelUid || q.GetModelAssignmentJwt() != "private-assignment" || !prepared.AssignmentUsed {
					t.Fatalf("ordinary router: %v %+v", err, prepared)
				}
			} else if !errors.As(err, &routeErr) || routeErr.Code != tc.code || q.ChatModelUid != ModelID(model) || q.ModelAssignmentJwt != nil {
				t.Fatalf("decision: %v; request UID %q", err, q.ChatModelUid)
			}
			if calls != tc.rpcs {
				t.Fatalf("RPCs = %d, want %d", calls, tc.rpcs)
			}
		})
	}
}

func TestModelEvidenceIsBoundedAndDoesNotChangeSnapshotAccounting(t *testing.T) {
	d := NewEventDecoder("public-alias", func(apicompat.ResponsesStreamEvent) error { return nil })
	for i := 0; i < MaxUsageObservations+5; i++ {
		if err := d.Apply(&pb.GetChatMessageResponse{MessageId: "private-session-message", ActualModelUid: proto.String("physical-model"), DeltaText: "private-source-code", DeltaThinking: "private-thinking", DeltaSignature: "private-signature", Usage: &pb.ModelUsageStats{ModelUid: "usage-model", RequestedModelUid: "requested-model", BillingModelUid: "billing-model", InputTokens: 12, OutputTokens: uint64(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.Evidence.Usage) != MaxUsageObservations || !d.Evidence.Truncated || len(d.Evidence.ActualModels) != 1 || d.Evidence.Usage[MaxUsageObservations-1].OutputTokens != MaxUsageObservations+4 || d.Response.Usage.InputTokens != 12 || d.Response.Usage.OutputTokens != MaxUsageObservations+4 {
		t.Fatal("evidence was unbounded or cumulative snapshots were added")
	}
	b, _ := json.Marshal(d.Evidence)
	for _, secret := range []string{"private-source-code", "private-thinking", "private-signature", "private-session-message"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatalf("diagnostic leaked %s", secret)
		}
	}
	for i := 0; i < MaxModelObservations+5; i++ {
		d.Evidence.Observe(&pb.GetChatMessageResponse{ActualModelUid: proto.String(strings.Repeat("x", i+1))})
	}
	if len(d.Evidence.ActualModels) != MaxModelObservations {
		t.Fatal("model observations unbounded")
	}
}

func TestUnexpectedSpecialResponseRetainsUsage(t *testing.T) {
	d := NewEventDecoder("ordinary", func(apicompat.ResponsesStreamEvent) error { return nil })
	err := d.Apply(&pb.GetChatMessageResponse{ActualModelUid: proto.String("fusion"), Usage: &pb.ModelUsageStats{InputTokens: 7, OutputTokens: 2}})
	var routeErr *RouteError
	if !errors.As(err, &routeErr) || routeErr.Code != CodeRouteMismatch || d.Response.Usage.InputTokens != 7 || d.Started {
		t.Fatalf("mismatch/usage: %v %+v", err, d.Response.Usage)
	}
}

func TestFinalModelCapabilities(t *testing.T) {
	model := &pb.ClientModelConfig{ModelUid: "limited", ModelInfo: &pb.ModelInfo{MaxOutputTokens: 64, ModelFeatures: &pb.ModelFeatures{}}}
	q := &pb.GetChatMessageRequest{Configuration: &pb.CompletionConfiguration{MaxTokens: 256}}
	if err := ValidateModelCapabilities(q, model, nil); err != nil || q.Configuration.MaxTokens != 64 {
		t.Fatal("default cap was not clamped", err)
	}
	explicit := 128
	if err := ValidateModelCapabilities(q, model, &explicit); err == nil {
		t.Fatal("explicit oversized output accepted")
	}
	q.ChatMessagePrompts = []*pb.ChatMessagePrompt{{Images: []*pb.ImageData{{Base64Data: "synthetic"}}}}
	if err := ValidateModelCapabilities(q, model, nil); err == nil {
		t.Fatal("image capability was not rechecked")
	}
	q.ChatMessagePrompts = nil
	q.Tools = []*pb.ChatToolDefinition{{Name: "tool"}}
	if err := ValidateModelCapabilities(q, model, nil); err == nil {
		t.Fatal("tool capability was not rechecked")
	}
	if err := ValidateModelCapabilities(q, &pb.ClientModelConfig{ModelUid: "legacy-without-features"}, nil); err != nil {
		t.Fatal("missing ordinary metadata was treated as an explicit denial")
	}
}
