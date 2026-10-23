package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"google.golang.org/protobuf/proto"
)

func TestClientIdentityDocumentValidation(t *testing.T) {
	if _, err := ParseClientIdentity(defaultClientIdentityJSON); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, data string }{
		{"missing fields", `{"schema_version":1,"revision":"partial"}`},
		{"unknown field", strings.Replace(string(defaultClientIdentityJSON), `"schema_version": 1`, `"api_key":"secret","schema_version": 1`, 1)},
		{"unsupported schema", strings.Replace(string(defaultClientIdentityJSON), `"schema_version": 1`, `"schema_version": 2`, 1)},
		{"trailing data", string(defaultClientIdentityJSON) + `{}`},
		{"oversized", strings.Repeat(" ", MaxClientIdentitySize+1)},
		{"malformed version", strings.Replace(string(defaultClientIdentityJSON), DefaultClientIdentity().Chat.IDEVersion, "latest", 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseClientIdentity([]byte(tc.data)); err == nil {
				t.Fatal("invalid identity was accepted")
			}
		})
	}
}

func TestClientIdentitySnapshotUsedAcrossRPCs(t *testing.T) {
	identity := DefaultClientIdentity()
	identity.Revision = "test-release-2"
	identity.Chat.IDEVersion, identity.Chat.ExtensionVersion = "4000.1.2", "4000.1.2"
	identity.Chat.OS, identity.Chat.Locale = "darwin", "zh-CN"
	identity.Discovery.IDEVersion, identity.Discovery.ExtensionVersion = "4000.1.3", "4000.1.3"
	identity.TokenExchange.IDEVersion, identity.TokenExchange.ExtensionVersion = "2.0.0", "2.0.0"
	client := &Client{BaseURL: DefaultBaseURL, Identity: &identity}
	observed := map[string]bool{}
	client.Do = func(r *http.Request) (*http.Response, error) {
		observed[r.URL.Path] = true
		body, _ := io.ReadAll(r.Body)
		var payload []byte
		checkChat := func(m *pb.Metadata) {
			t.Helper()
			if m.GetIdeVersion() != "4000.1.2" || m.GetExtensionVersion() != "4000.1.2" || m.GetOs() != "darwin" || m.GetLocale() != "zh-CN" {
				t.Fatalf("chat RPC did not use the configured identity: %v", m)
			}
		}
		switch r.URL.Path {
		case UserJWTPath:
			var q pb.GetUserJwtRequest
			_ = proto.Unmarshal(body, &q)
			checkChat(q.Metadata)
			payload, _ = proto.Marshal(&pb.GetUserJwtResponse{UserJwt: "jwt"})
		case AssignPath:
			var q pb.AssignModelRequest
			_ = proto.Unmarshal(body, &q)
			checkChat(q.Metadata)
			payload, _ = proto.Marshal(&pb.AssignModelResponse{Assignment: &pb.ModelAssignment{ModelUid: "actual", AssignmentJwt: "assignment"}})
		case ModelsPath:
			var q pb.GetCliModelConfigsRequest
			_ = proto.Unmarshal(body, &q)
			if q.GetMetadata().GetIdeVersion() != "4000.1.3" || len(q.GetMetadata().GetSupportedModelDisplays()) != 5 {
				t.Fatal("model discovery lost its configured identity or protocol capabilities")
			}
			payload, _ = proto.Marshal(&pb.GetCliModelConfigsResponse{})
		case MintPath:
			var q struct {
				Metadata struct {
					ClientInfo
					APIKey string `json:"api_key"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(body, &q); err != nil || q.Metadata.IDEVersion != "2.0.0" || q.Metadata.APIKey != "cog_test" {
				t.Fatalf("token exchange lost identity or credentials: %v", err)
			}
			payload = []byte(`{"sessionToken":"minted"}`)
		case ChatPath:
			_, raw, err := ReadFrame(bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			var q pb.GetChatMessageRequest
			_ = proto.Unmarshal(raw, &q)
			checkChat(q.Metadata)
			payload = frame(2, []byte(`{}`))
		default:
			t.Fatalf("unexpected RPC %s", r.URL.Path)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(payload))}, nil
	}
	ctx := context.Background()
	chat := &pb.GetChatMessageRequest{ChatModelUid: "router"}
	plan := PlanModel(&pb.ClientModelConfig{ModelUid: chat.ChatModelUid, ModelInfo: &pb.ModelInfo{IsModelRouter: true}})
	if _, err := client.Prepare(ctx, "test", chat, plan, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Models(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Mint(ctx, "cog_test"); err != nil {
		t.Fatal(err)
	}
	if err := client.Chat(ctx, client.BaseURL, "test", chat, func(*pb.GetChatMessageResponse) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(observed) != 5 {
		t.Fatal("not all identity paths were exercised")
	}
}
