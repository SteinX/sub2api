//go:build devine2e

package devine2e

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	server     *httptest.Server
	mints      atomic.Int32
	rejectJWT  atomic.Bool
	failChat   atomic.Bool
	failStream atomic.Bool
	mu         sync.Mutex
	chats      []*pb.GetChatMessageRequest
}

func newNativeFixture(t *testing.T) *nativeFixture {
	f := &nativeFixture{}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
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
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"sessionToken": "devin-session-token$fixture-session-" + strconv.Itoa(int(f.mints.Load()))})
	case devin.ModelsPath:
		writeProto(&pb.GetCliModelConfigsResponse{ClientModelConfigs: []*pb.ClientModelConfig{{
			ModelUid: fixtureModel, Label: "E2E synthetic model", MaxTokens: 200000,
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
