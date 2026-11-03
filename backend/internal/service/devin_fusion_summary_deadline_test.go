//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Use public tool-result continuation, actual native framing and durable Redis
// state. Only completed historical tools are seeded to reach the hard gate.
func TestFusionGatewaySummaryCancellationCollectsBeforeStopping(t *testing.T) {
	for _, final := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial_summary", true: "completed_summary"}[final], func(t *testing.T) {
			f := newWarmFixture(t)
			t.Cleanup(func() { _ = f.service.CloseDevinCacheWarm(context.Background()) })
			p := f.session.Profile
			p.MaxContextBytes = 24000
			p.Compaction = &fusion.CompactionPolicy{Enabled: true, Sidekick: []int{6000, 12000, 18000}, MaxAttempts: 2, TimeoutSeconds: 30, MaxOutputTokens: 1024}
			f.account.Extra[fusion.ExtraProfiles] = []fusion.Profile{p}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.chat = func(_ *http.Request, wire *pb.GetChatMessageRequest) (*http.Response, error) {
				switch f.calls.Load() {
				case 1, 2:
					return fusionRecoveryToolReply(wire.ChatModelUid, int(f.calls.Load())), nil
				case 3:
					require.Empty(t, wire.Tools, "the interrupted call must be the internal summary")
					require.Equal(t, p.SidekickModel, wire.ChatModelUid)
					m := &pb.GetChatMessageResponse{StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN, Usage: &pb.ModelUsageStats{InputTokens: 100, OutputTokens: 20, BillingModelUid: wire.ChatModelUid}, DeltaText: `{"goal":"Preserve API","current_state":"Historical evidence retained","decisions":[],"changes":[],"evidence":["Observed old tools"],"checks":[],"open_questions":[],"next_steps":[],"files":[]}`}
					cancel()
					if !final {
						return interruptedAccountingReply(t, m, context.DeadlineExceeded), nil
					}
					return warmReply(m), nil
				case 4, 5:
					require.True(t, final, "an unknown summary cannot dispatch another role")
					text := "Actual Sidekick report: original tool result verified"
					if wire.ChatModelUid == p.LeadModel {
						text = "Lead reviewed the actual Sidekick report"
					}
					return warmReply(&pb.GetChatMessageResponse{StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN, DeltaText: text, Usage: &pb.ModelUsageStats{InputTokens: 100, OutputTokens: 20, BillingModelUid: wire.ChatModelUid}}), nil
				default:
					t.Errorf("unexpected repeated generation %d", f.calls.Load())
					return devinReply(500, nil), nil
				}
			}
			request := func(ctx context.Context, body []byte, id string) (*httptest.ResponseRecorder, error) {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
				c.Request.Header.Set(fusion.HeaderSession, "summary-deadline-session")
				c.Request.Header.Set("Idempotency-Key", id)
				c.Set("api_key", f.keys.key)
				_, err := f.service.forwardDevin(c.Request.Context(), c, f.account, body, "responses", "", "")
				return w, err
			}
			first, err := request(context.Background(), fusionProtocolBody("responses", false, ""), "initial")
			require.NoError(t, err)
			callID := regexp.MustCompile(`call_fusion_[0-9a-f-]+`).FindString(first.Body.String())
			require.NotEmpty(t, callID)
			scope := fusion.Scope(f.keys.key.UserID, f.keys.key.ID)
			s, _, err := f.engine.Load(context.Background(), scope, "summary-deadline-session")
			require.NoError(t, err)
			require.NotNil(t, s.Pending)
			started := s.TurnStarted
			pending := s.Sidekick[len(s.Sidekick)-1]
			s.Sidekick = s.Sidekick[:len(s.Sidekick)-1]
			for i := range 12 {
				id := fmt.Sprintf("historical-%d", i)
				s.Sidekick = append(s.Sidekick, apicompat.ResponsesInputItem{Type: "function_call", CallID: id, Name: "read_file", Arguments: `{}`}, apicompat.ResponsesInputItem{Type: "function_call_output", CallID: id, Output: strings.Repeat("historical evidence ", 80)})
			}
			s.Sidekick = append(s.Sidekick, pending)
			data, err := json.Marshal(s)
			require.NoError(t, err)
			require.NoError(t, f.engine.Store.Set(context.Background(), "devin:fusion:session:"+scope+":"+fusion.Digest(s.ID), data, time.Hour))
			body := fusionProtocolBody("responses", false, callID)
			_, err = request(ctx, body, "tool-result")
			require.Error(t, err)
			require.EqualValues(t, 3, f.calls.Load(), "only the summary may run before interruption")
			s, _, err = f.engine.Load(context.Background(), scope, s.ID)
			require.NoError(t, err)
			require.True(t, started.Equal(s.TurnStarted))
			require.Equal(t, 3, s.TurnRequests)
			require.Equal(t, 900, s.Profile.MaxWallSeconds)
			require.Equal(t, 30, s.Profile.CompactPolicy().TimeoutSeconds)
			if !final {
				require.Equal(t, fusion.Uncertain, s.State)
				require.Greater(t, s.AuxReservedUSD, 0.0)
				require.Len(t, f.bills.commands, 2, "partial summary stats must not be billed")
				require.Equal(t, 1, f.cache.count())
				_, err = request(context.Background(), body, "tool-result")
				require.Error(t, err)
				require.EqualValues(t, 3, f.calls.Load())
				return
			}
			require.Equal(t, fusion.RunningSidekick, s.State)
			require.GreaterOrEqual(t, s.ActiveRequest, 0)
			require.Zero(t, s.AuxReservedUSD)
			require.Len(t, f.bills.commands, 3)
			require.Zero(t, f.cache.count())
			last, err := request(context.Background(), body, "tool-result")
			if err != nil {
				state, _, loadErr := f.engine.Load(context.Background(), scope, s.ID)
				require.NoError(t, loadErr)
				job, _ := json.Marshal(state.Contexts[fusion.Sidekick])
				t.Logf("resume summary state: %s", job)
			}
			require.NoError(t, err)
			require.Contains(t, last.Body.String(), "Lead reviewed the actual Sidekick report")
			require.EqualValues(t, 5, f.calls.Load())
			require.Len(t, f.bills.commands, 5, "each physical receipt settles once")
			s, _, err = f.engine.Load(context.Background(), scope, s.ID)
			require.NoError(t, err)
			require.Equal(t, fusion.Completed, s.State)
			require.True(t, started.Equal(s.TurnStarted))
			require.Equal(t, 5, s.TurnRequests)
			require.Equal(t, 1, s.Turn)
		})
	}
}
