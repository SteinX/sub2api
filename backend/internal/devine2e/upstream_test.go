//go:build devine2e

package devine2e

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"google.golang.org/protobuf/proto"
)

const fixtureModel = "swe-2-max"

// This server speaks native Connect/protobuf over a real socket. The gateway,
// scheduler, authentication, repositories and billing are the production binary.
type nativeFixture struct {
	server              *httptest.Server
	proxy               *httptest.Server
	ca                  []byte
	credentialReference string
	referenceExchanges  atomic.Int32
	mints               atomic.Int32
	denyMint            atomic.Bool
	denyModels          atomic.Bool
	emptyModels         atomic.Bool
	rejectJWT           atomic.Bool
	failChat            atomic.Bool
	failStream          atomic.Bool
	mu                  sync.Mutex
	chats               []*pb.GetChatMessageRequest
}

func newNativeFixture(t *testing.T) *nativeFixture {
	return newNativeFixtureWithReference(t, "")
}

func newNativeFixtureWithReference(t *testing.T, reference string) *nativeFixture {
	t.Helper()
	f := &nativeFixture{credentialReference: reference}
	f.startTLSProxy(t)
	return f
}

func (f *nativeFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", 405)
		return
	}
	writeProto := func(m proto.Message) {
		b, _ := proto.Marshal(m)
		w.Header().Set("Content-Type", "application/proto")
		_, _ = w.Write(b)
	}
	switch r.URL.Path {
	case devin.MintPath:
		f.mints.Add(1)
		if f.denyMint.Load() {
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"code":"permission_denied","message":"synthetic denied credential"}`)
			return
		}
		var body struct {
			Metadata struct {
				APIKey string `json:"api_key"`
			} `json:"metadata"`
		}
		key := r.Header.Get("X-Api-Key")
		isReference := f.credentialReference != "" && key == f.credentialReference
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body) != nil || (!strings.HasPrefix(key, "cog_e2e_") && !isReference) || body.Metadata.APIKey != key {
			http.Error(w, "invalid synthetic PAT exchange", 401)
			return
		}
		if isReference {
			f.referenceExchanges.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionToken": "devin-session-token$fixture-session-" + strconv.Itoa(int(f.mints.Load()))})
	case devin.ModelsPath:
		if f.denyModels.Load() {
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"code":"permission_denied"}`)
			return
		}
		if f.emptyModels.Load() {
			writeProto(&pb.GetCliModelConfigsResponse{})
			return
		}
		var query pb.GetCliModelConfigsRequest
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		user, password, ok := r.BasicAuth()
		if err != nil || proto.Unmarshal(body, &query) != nil || !ok || user != password || !strings.HasPrefix(user, "devin-session-token$fixture-session-") || query.GetMetadata().GetApiKey() != user {
			http.Error(w, "invalid synthetic model discovery authentication", 401)
			return
		}
		writeProto(&pb.GetCliModelConfigsResponse{ClientModelConfigs: []*pb.ClientModelConfig{
			{ModelUid: "e2e-expensive", CreditMultiplier: 8, ModelInfo: &pb.ModelInfo{MaxOutputTokens: 32000}},
			{ModelUid: "e2e-opaque-cheap", CreditMultiplier: .01, ModelInfo: &pb.ModelInfo{HarnessUids: []string{"unknown-harness"}, MaxOutputTokens: 32000}},
			{
				ModelUid: fixtureModel, Label: "E2E synthetic model", MaxTokens: 200000, CreditMultiplier: 1,
				ModelInfo: &pb.ModelInfo{ModelUid: fixtureModel, MaxOutputTokens: 32000,
					ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}},
			}}})
	case devin.UserJWTPath:
		if f.rejectJWT.Swap(false) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"code":"unauthenticated"}`)
			return
		}
		writeProto(&pb.GetUserJwtResponse{UserJwt: "fixture-user-jwt"})
	case devin.ChatPath:
		_, body, err := devin.ReadFrame(r.Body)
		var request pb.GetChatMessageRequest
		if err != nil || proto.Unmarshal(body, &request) != nil || request.GetMetadata().GetUserJwt() != "fixture-user-jwt" || request.ChatModelUid != fixtureModel {
			http.Error(w, "invalid native request", 400)
			return
		}
		f.mu.Lock()
		f.chats = append(f.chats, &request)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/connect+proto")
		if f.failChat.Swap(false) {
			writeFrame(w, 2, []byte(`{"error":{"code":"invalid_argument","message":"fixture failure"}}`))
			return
		}
		if f.failStream.Swap(false) {
			b, _ := proto.Marshal(&pb.GetChatMessageResponse{DeltaText: "partial"})
			writeFrame(w, 0, b)
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			writeFrame(w, 2, []byte(`{"error":{"code":"invalid_argument","message":"fixture failure after output"}}`))
			return
		}
		reply := &pb.GetChatMessageResponse{MessageId: "fixture-message", DeltaText: "E2E_OK",
			StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN,
			Usage:      &pb.ModelUsageStats{InputTokens: 11, OutputTokens: 3, CacheReadTokens: 5, CacheWriteTokens: 2, BillingModelUid: fixtureModel}}
		resultSeen, reportSeen := false, false
		for _, prompt := range request.ChatMessagePrompts {
			if prompt.ToolCallId == "call_e2e" && prompt.Prompt == "E2E_TOOL_RESULT" {
				resultSeen = true
			}
			if prompt.ToolCallId == "call_e2e_delegate" {
				reportSeen = true
			}
		}
		clientTool, delegateTool := false, false
		for _, tool := range request.Tools {
			if tool.Name == "e2e_echo" {
				clientTool = true
			}
			if tool.Name == fusion.DelegateTool {
				delegateTool = true
			}
		}
		if delegateTool && !reportSeen {
			reply.DeltaText = ""
			reply.DeltaToolCalls = []*pb.ChatToolCall{{Id: "call_e2e_delegate", Name: fusion.DelegateTool,
				ArgumentsJson: `{"task":"Verify the synthetic E2E task","constraints":"Use only synthetic data","acceptance_criteria":"Report E2E_OK"}`}}
			reply.StopReason = pb.StopReason_STOP_REASON_FUNCTION_CALL
		} else if clientTool && !resultSeen {
			reply.DeltaText = ""
			reply.DeltaToolCalls = []*pb.ChatToolCall{{Id: "call_e2e", Name: "e2e_echo", ArgumentsJson: `{"value":"E2E_TOOL_RESULT"}`}}
			reply.StopReason = pb.StopReason_STOP_REASON_FUNCTION_CALL
		}
		b, _ := proto.Marshal(reply)
		writeFrame(w, 0, b)
		writeFrame(w, 2, []byte(`{}`))
	default:
		http.Error(w, "unexpected RPC", 404)
	}
}

func writeFrame(w io.Writer, flags byte, body []byte) {
	header := make([]byte, 5)
	header[0] = flags
	binary.BigEndian.PutUint32(header[1:], uint32(len(body)))
	_, _ = w.Write(header)
	_, _ = w.Write(body)
}
