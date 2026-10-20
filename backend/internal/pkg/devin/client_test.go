package devin

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"google.golang.org/protobuf/proto"
)

func frame(flags byte, body []byte) []byte {
	h := make([]byte, 5)
	h[0] = flags
	binary.BigEndian.PutUint32(h[1:], uint32(len(body)))
	return append(h, body...)
}
func TestConnectStreamContract(t *testing.T) {
	for _, tc := range []struct {
		name      string
		tail      []byte
		wantError bool
	}{{"success", frame(2, []byte(`{}`)), false}, {"rejection", frame(2, []byte(`{"error":{"code":"resource_exhausted","message":"do not echo upstream payload"}}`)), true}, {"missing trailer", nil, true}} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Connect-Content-Encoding") != "gzip" {
					t.Error("missing frame compression header")
				}
				flags, b, err := ReadFrame(r.Body)
				if err != nil || flags != 1 {
					t.Errorf("request frame: %d %v", flags, err)
				}
				var req pb.GetChatMessageRequest
				if err = proto.Unmarshal(b, &req); err != nil {
					t.Fatal(err)
				}
				if req.ChatModelUid != "swe-2-max" {
					t.Errorf("model %q", req.ChatModelUid)
				}
				body, _ := proto.Marshal(&pb.GetChatMessageResponse{DeltaText: "hello", StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN})
				_, _ = w.Write(frame(0, body))
				_, _ = w.Write(tc.tail)
			}))
			defer server.Close()
			c := &Client{BaseURL: server.URL, Do: server.Client().Do}
			count := 0
			err := c.Chat(context.Background(), server.URL, "devin-session-token$test", &pb.GetChatMessageRequest{ChatModelUid: "swe-2-max"}, func(m *pb.GetChatMessageResponse) error {
				count++
				if m.DeltaText != "hello" {
					t.Error("missing text")
				}
				return nil
			})
			if (err != nil) != tc.wantError {
				t.Errorf("error = %v", err)
			}
			if count != 1 {
				t.Errorf("events %d", count)
			}
			if tc.name == "rejection" {
				var e *Error
				if !errors.As(err, &e) || e.HTTPStatus() != 429 {
					t.Fatalf("lost Connect code: %v", err)
				}
				if strings.Contains(err.Error(), "echo") {
					t.Error("untrusted trailer leaked")
				}
			}
		})
	}
}
func TestReadFrameBoundsAndCompression(t *testing.T) {
	var zipped bytes.Buffer
	z := gzip.NewWriter(&zipped)
	_, _ = z.Write([]byte("payload"))
	_ = z.Close()
	_, b, err := ReadFrame(bytes.NewReader(frame(1, zipped.Bytes())))
	if err != nil || string(b) != "payload" {
		t.Fatal(err, string(b))
	}
	h := make([]byte, 5)
	binary.BigEndian.PutUint32(h[1:], MaxFrameSize+1)
	if _, _, err = ReadFrame(bytes.NewReader(h)); err == nil {
		t.Fatal("oversized frame accepted")
	}
	if _, _, err = ReadFrame(bytes.NewReader(frame(4, nil))); err == nil {
		t.Fatal("unknown flags accepted")
	}
	if _, _, err = ReadFrame(bytes.NewReader(frame(0, []byte("a"))[:5])); !errors.Is(err, io.EOF) {
		t.Fatalf("truncated payload: %v", err)
	}
}
func TestUserJWTAndRouterWireContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		switch r.URL.Path {
		case UserJWTPath:
			var q pb.GetUserJwtRequest
			_ = proto.Unmarshal(b, &q)
			if q.GetMetadata().GetApiKey() != "devin-session-token$test" {
				t.Error("missing CLI auth")
			}
			if m := q.GetMetadata(); m.GetIdeName() != "devin-cli" || m.GetIdeType() != "chisel" || m.GetIdeVersion() != "3000.6.2" || m.GetExtensionName() != "chisel" || m.GetExtensionVersion() != "3000.6.2" {
				t.Error("missing released CLI identity")
			}
			out, _ := proto.Marshal(&pb.GetUserJwtResponse{UserJwt: "jwt"})
			_, _ = w.Write(out)
		case AssignPath:
			var q pb.AssignModelRequest
			_ = proto.Unmarshal(b, &q)
			if q.Metadata.UserJwt != "jwt" || q.CascadeId != "cascade" {
				t.Error("router auth/session lost")
			}
			out, _ := proto.Marshal(&pb.AssignModelResponse{Assignment: &pb.ModelAssignment{ModelUid: "actual", AssignmentJwt: "assignment"}})
			_, _ = w.Write(out)
		default:
			t.Error("unexpected RPC", r.URL.Path)
		}
	}))
	defer server.Close()
	c := &Client{BaseURL: server.URL, Do: server.Client().Do}
	q := &pb.GetChatMessageRequest{ChatModelUid: "router", CascadeId: "cascade"}
	base, err := c.Prepare(context.Background(), "test", q, true)
	if err != nil || base != server.URL || q.ChatModelUid != "actual" || q.GetModelAssignmentJwt() != "assignment" {
		t.Fatal(err, q)
	}
	f := q.Metadata.ProtoReflect().Descriptor().Fields().ByName("user_jwt")
	if f.Number() != 21 {
		t.Fatalf("wrong user_jwt field number: %d", f.Number())
	}
}

func TestModelDiscoveryWireContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ModelsPath {
			t.Error("unexpected discovery endpoint", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		var q pb.GetCliModelConfigsRequest
		if err := proto.Unmarshal(b, &q); err != nil {
			t.Error(err)
		}
		m := q.GetMetadata()
		if m.GetApiKey() != "devin-session-token$test" || m.GetIdeName() != "chisel" || m.GetIdeVersion() != "0.0.0-dev" || m.GetExtensionVersion() != "0.0.0-dev" {
			t.Error("missing discovery identity")
		}
		if !slices.Equal(m.GetSupportedModelDisplays(), []pb.DisplayOption{3, 4, 6, 7, 8}) {
			t.Error("discovery omitted model catalog slots")
		}
		out := &pb.GetCliModelConfigsResponse{ClientModelConfigs: []*pb.ClientModelConfig{
			{ModelUid: "router", ModelInfo: &pb.ModelInfo{DisplayOption: pb.DisplayOption_DISPLAY_OPTION_MODEL_ROUTER}},
			{ModelUid: "normal", ModelInfo: &pb.ModelInfo{DisplayOption: pb.DisplayOption_DISPLAY_OPTION_NORMAL}},
			{ModelUid: "quick", ModelInfo: &pb.ModelInfo{DisplayOption: pb.DisplayOption_DISPLAY_OPTION_QUICK_REVIEW}},
			{ModelUid: "internal", ModelInfo: &pb.ModelInfo{DisplayOption: pb.DisplayOption_DISPLAY_OPTION_INTERNAL_DEFAULT}},
			{ModelUid: "disabled", Disabled: true},
			{ModelUid: "normal"},
		}}
		b, _ = proto.Marshal(out)
		_, _ = w.Write(b)
	}))
	defer server.Close()
	c := &Client{BaseURL: server.URL, Do: server.Client().Do}
	models, err := c.Models(context.Background(), "test")
	if err != nil || len(models) != 2 || ModelID(models[0]) != "router" || ModelID(models[1]) != "normal" {
		t.Fatalf("discovered models: %v, %v", models, err)
	}
}
func TestRequestHistoryAndTenantIsolation(t *testing.T) {
	r := &apicompat.ResponsesRequest{Model: "swe-2-max", Input: json.RawMessage(`[{"role":"system","content":"system"},{"role":"user","content":"question"},{"type":"function_call","call_id":"a","name":"one","arguments":"{}"},{"type":"function_call","call_id":"b","name":"two","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"first"},{"type":"function_call_output","call_id":"b","output":"second"}]`)}
	q, err := BuildRequest(r, "tenant1:session")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := BuildRequest(r, "tenant1:session")
	other, _ := BuildRequest(r, "tenant2:session")
	if q.CascadeId != again.CascadeId || q.CascadeId == other.CascadeId {
		t.Fatal("unstable or cross-tenant cascade id")
	}
	if q.ExecutionId == again.ExecutionId {
		t.Fatal("execution id reused")
	}
	if q.Prompt != "\nsystem" || len(q.ChatMessagePrompts) != 5 {
		t.Fatal(q)
	}
	if q.ChatMessagePrompts[2].ToolCallId != "a" || q.ChatMessagePrompts[4].ToolCallId != "b" {
		t.Fatal("tool results were not paired")
	}
	r.PreviousResponseID = "resp_previous"
	if _, err = BuildRequest(r, "x"); err == nil {
		t.Fatal("unsupported partial history accepted")
	}
}
func TestDecoderToolFragmentsUsageAndFailure(t *testing.T) {
	var events []apicompat.ResponsesStreamEvent
	d := NewEventDecoder("alias", func(e apicompat.ResponsesStreamEvent) error { events = append(events, e); return nil })
	for _, m := range []*pb.GetChatMessageResponse{{MessageId: "msg", DeltaThinking: "reason"}, {DeltaText: "answer"}, {DeltaToolCalls: []*pb.ChatToolCall{{Id: "call", Name: "tool", ArgumentsJson: `{"x":`}}}, {DeltaToolCalls: []*pb.ChatToolCall{{ArgumentsJson: `1}`}}, Usage: &pb.ModelUsageStats{InputTokens: 10, CacheWriteTokens: 2, CacheReadTokens: 5, OutputTokens: 3}, StopReason: pb.StopReason_STOP_REASON_FUNCTION_CALL}} {
		if err := d.Apply(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Finish(); err != nil {
		t.Fatal(err)
	}
	if d.Response.Usage.InputTokens != 17 || d.Response.Usage.TotalTokens != 20 {
		t.Fatal(d.Response.Usage)
	}
	tool := d.Response.Output[2]
	if tool.Arguments != `{"x":1}` || tool.CallID != "call" {
		t.Fatal(tool)
	}
	if events[len(events)-1].Type != "response.completed" {
		t.Fatal("missing terminal event")
	}
	for i, e := range events {
		if e.SequenceNumber != i {
			t.Fatal("event sequence")
		}
	}
	broken := NewEventDecoder("model", func(apicompat.ResponsesStreamEvent) error { return nil })
	_ = broken.Apply(&pb.GetChatMessageResponse{DeltaToolCalls: []*pb.ChatToolCall{{Id: "id", Name: "tool", ArgumentsJson: `{`}}})
	if broken.Finish() == nil {
		t.Fatal("partial JSON accepted")
	}
}
