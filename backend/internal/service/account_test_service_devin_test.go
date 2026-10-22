//go:build unit

package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func devinProtoReply(t *testing.T, message proto.Message) *http.Response {
	t.Helper()
	body, err := proto.Marshal(message)
	assert.NoError(t, err)
	return devinReply(http.StatusOK, body)
}

func TestDevinAccountProbeUsesNativeGenerationConfiguration(t *testing.T) {
	for _, router := range []bool{false, true} {
		t.Run(map[bool]string{false: "direct", true: "router"}[router], func(t *testing.T) {
			gateway := devinCompatGateway(t, func(q *pb.GetChatMessageRequest) []*pb.GetChatMessageResponse {
				assert.Equal(t, "hi", q.ChatMessagePrompts[0].Prompt)
				assert.Equal(t, uint64(64000), q.Configuration.MaxTokens, "account probes need the same reasoning budget as real requests")
				assert.Equal(t, uint64(40), q.Configuration.TopK)
				assert.Equal(t, uint64(400), q.Configuration.MaxNewlines)
				assert.Equal(t, 1.0, q.Configuration.Temperature)
				assert.Equal(t, "user-jwt", q.Metadata.UserJwt)
				if router {
					assert.Equal(t, "swe-2-high", q.ChatModelUid)
					assert.Equal(t, "assigned", q.GetModelAssignmentJwt())
				}
				return []*pb.GetChatMessageResponse{{DeltaText: "OK", StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN}}
			})
			upstream := gateway.devin.upstream.(*devinHTTP)
			original := upstream.handle
			upstream.handle = func(r *http.Request) (*http.Response, error) {
				if router && r.URL.Path == devin.ModelsPath {
					return devinProtoReply(t, &pb.GetCliModelConfigsResponse{ClientModelConfigs: []*pb.ClientModelConfig{{ModelUid: "router-test", ModelInfo: &pb.ModelInfo{IsModelRouter: true}}}}), nil
				}
				if r.URL.Path == devin.AssignPath {
					return devinProtoReply(t, &pb.AssignModelResponse{Assignment: &pb.ModelAssignment{ModelUid: "swe-2-high", AssignmentJwt: "assigned"}}), nil
				}
				return original(r)
			}
			probe := &AccountTestService{devin: gateway.devin}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
			account := &Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeAPIKey, Credentials: map[string]any{"api_key": "cog_test"}}
			model := "swe-2-max"
			if router {
				model = "router-test"
			}
			require.NoError(t, probe.testDevinAccountConnection(c, account, model, "hi"))
			require.Contains(t, w.Body.String(), `"success":true`)
			require.Contains(t, w.Body.String(), `"text":"OK"`)
		})
	}
}

func TestDevinAccountProbeReportsAuthStageWithoutFalseConnectedEvent(t *testing.T) {
	service, _, _ := devinTestService(t)
	upstream := service.upstream.(*devinHTTP)
	original := upstream.handle
	upstream.handle = func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == devin.UserJWTPath {
			return devinReply(400, []byte(`{"code":"invalid_argument","message":"internal error (trace ID: 0123456789abcdef0123456789abcdef)"}`)), nil
		}
		return original(r)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)
	probe := &AccountTestService{devin: service}
	account := &Account{ID: 1, Platform: PlatformDevin, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "sk-ws-test"}}
	require.Error(t, probe.testDevinAccountConnection(c, account, "swe-2-max", "hi"))
	require.Contains(t, w.Body.String(), "GetUserJwt")
	require.Contains(t, w.Body.String(), "0123456789abcdef0123456789abcdef")
	require.NotContains(t, w.Body.String(), "test_start")
	require.NotContains(t, w.Body.String(), `"success":true`)
}
