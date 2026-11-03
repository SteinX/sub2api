//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	mc "github.com/Wei-Shaw/sub2api/internal/pkg/managedcontext"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

type accountingReadError struct{ err error }

func (r accountingReadError) Read([]byte) (int, error) { return 0, r.err }

func interruptedAccountingReply(t *testing.T, m *pb.GetChatMessageResponse, err error) *http.Response {
	t.Helper()
	b, marshalErr := proto.Marshal(m)
	require.NoError(t, marshalErr)
	r := devinReply(200, nil)
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(devinTestFrame(0, b)), accountingReadError{err}))
	return r
}

// Exercise public Responses admission, native framing, the persisted Fusion
// attempt and real billing pipeline, with only the upstream transport mocked.
func TestFusionGatewayFinalUsageSurvivesOutputErrorsButNotBrokenStreams(t *testing.T) {
	for _, kind := range []string{"partial_deadline", "stop_then_broken_stream", "terminal_malformed_tool", "terminal_refusal", "terminal_incomplete"} {
		t.Run(kind, func(t *testing.T) {
			f := newWarmFixture(t)
			t.Cleanup(func() { _ = f.service.CloseDevinCacheWarm(context.Background()) })
			final := kind != "partial_deadline" && kind != "stop_then_broken_stream"
			f.chat = func(_ *http.Request, wire *pb.GetChatMessageRequest) (*http.Response, error) {
				m := &pb.GetChatMessageResponse{DeltaText: "observed output", StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN, Usage: &pb.ModelUsageStats{InputTokens: 9, OutputTokens: 2, BillingModelUid: wire.ChatModelUid}}
				switch kind {
				case "partial_deadline":
					m.StopReason = pb.StopReason_STOP_REASON_UNSPECIFIED
					return interruptedAccountingReply(t, m, context.DeadlineExceeded), nil
				case "stop_then_broken_stream":
					return interruptedAccountingReply(t, m, io.ErrUnexpectedEOF), nil
				case "terminal_malformed_tool":
					m.DeltaToolCalls = []*pb.ChatToolCall{{Id: "invalid-call", Name: fusion.DelegateTool, ArgumentsJson: `{`}}
				case "terminal_refusal":
					m.StopReason = pb.StopReason_STOP_REASON_CONTENT_FILTER
				case "terminal_incomplete":
					m.StopReason = pb.StopReason_STOP_REASON_MAX_TOKENS
				}
				return warmReply(m), nil
			}
			request := func() (*httptest.ResponseRecorder, error) {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
				c.Request.Header.Set(fusion.HeaderSession, "accounting-session")
				c.Request.Header.Set("Idempotency-Key", "original-user-task")
				c.Set("api_key", f.keys.key)
				_, err := f.service.forwardDevin(c.Request.Context(), c, f.account, fusionProtocolBody("responses", true, ""), "responses", "", "")
				return w, err
			}
			_, err := request()
			require.Error(t, err)
			scope := fusion.Scope(f.keys.key.UserID, f.keys.key.ID)
			s, _, err := f.engine.Load(context.Background(), scope, "accounting-session")
			require.NoError(t, err)
			require.Len(t, s.Attempts, 1)
			a := s.Attempts[0]
			require.NotNil(t, a.Response.Usage)
			require.NotNil(t, a.UsageFinal)
			require.Equal(t, final, *a.UsageFinal)
			require.Equal(t, final, a.Settled)
			if final {
				require.Equal(t, "completed", a.Status)
				require.Len(t, f.bills.commands, 1)
				require.Zero(t, f.cache.count())
				if kind != "terminal_incomplete" {
					require.Equal(t, "in_progress", a.Response.Status, "output failure must not discard final accounting")
				}
			} else {
				require.Equal(t, "unknown", a.Status)
				require.Equal(t, fusion.Uncertain, s.State)
				require.Empty(t, f.bills.commands)
				require.Equal(t, 1, f.cache.count(), "unknown physical usage keeps its reservation")
			}
			_, err = request()
			require.Error(t, err)
			require.EqualValues(t, 1, f.calls.Load(), "recovery must not send another generation")
			if final {
				require.Len(t, f.bills.commands, 1, "terminal receipts are charged exactly once")
			}
		})
	}
}

func TestFusionWarmStopBeforeBrokenTransportDoesNotSettlePartialUsage(t *testing.T) {
	f := newWarmFixture(t)
	f.chat = func(_ *http.Request, wire *pb.GetChatMessageRequest) (*http.Response, error) {
		m := &pb.GetChatMessageResponse{StopReason: pb.StopReason_STOP_REASON_MAX_TOKENS, Usage: &pb.ModelUsageStats{InputTokens: 4, CacheReadTokens: 96, OutputTokens: 1, BillingModelUid: wire.ChatModelUid}}
		return interruptedAccountingReply(t, m, context.DeadlineExceeded), nil
	}
	estimate, err := f.adapter.Estimate(context.Background(), f.snapshot)
	require.NoError(t, err)
	a := fusion.Attempt{ID: "unknown-warm", Purpose: fusion.CacheRefresh, Role: fusion.Lead, Model: f.snapshot.Model, StartedAt: f.session.TurnStarted, EstimateUSD: estimate}
	result, err := f.adapter.Refresh(context.Background(), f.snapshot, a)
	require.Error(t, err)
	require.False(t, result.Outcome.UsageFinal)
	require.True(t, result.UsageUncertain)
	a.Response, a.BillingModel, a.UsageFinal = result.Outcome.Response, result.Outcome.BillingModel, &result.Outcome.UsageFinal
	_, err = f.adapter.Settle(context.Background(), f.snapshot, a)
	require.ErrorContains(t, err, "usage is not final")
	require.Empty(t, f.bills.commands)
	require.Equal(t, 1, f.cache.count())
	require.EqualValues(t, 1, f.calls.Load())
}

func TestContextGatewayPreservesNativeFinalUsageEvidence(t *testing.T) {
	for _, final := range []bool{false, true} {
		t.Run(map[bool]string{false: "partial_deadline", true: "terminal_invalid_tool"}[final], func(t *testing.T) {
			f := newWarmFixture(t)
			model := f.session.Profile.LeadModel
			f.keys.key.User.Status = StatusActive
			f.account.Extra[DevinContextEnabledExtra] = true
			f.service.cfg.Gateway.DevinContext.Enabled = true
			f.service.cfg.Gateway.DevinContext.APIKeyIDs = []int64{f.keys.key.ID}
			f.service.cfg.Gateway.DevinContext.ModelUIDs = []string{model}
			f.service.concurrencyService = NewConcurrencyService(&stubConcurrencyCacheForTest{acquireResult: true})
			store := newContextGatewayStore()
			f.service.devin.contextStore = store
			keys := &APIKeyService{apiKeyRepo: &apiKeyRepoStub{apiKey: f.keys.key}}
			f.chat = func(_ *http.Request, wire *pb.GetChatMessageRequest) (*http.Response, error) {
				m := &pb.GetChatMessageResponse{Usage: &pb.ModelUsageStats{InputTokens: 9, OutputTokens: 2, BillingModelUid: wire.ChatModelUid}, DeltaToolCalls: []*pb.ChatToolCall{{Id: "invalid", Name: "read_file", ArgumentsJson: `{`}}}
				if !final {
					return interruptedAccountingReply(t, m, context.DeadlineExceeded), nil
				}
				return warmReply(m), nil
			}
			body, err := json.Marshal(map[string]any{"model": model, "input": "preserve original task", "max_output_tokens": 128, "tools": []map[string]any{{"type": "function", "name": "read_file", "parameters": map[string]any{"type": "object"}}}})
			require.NoError(t, err)
			request := func() error {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
				c.Request.Header.Set("Idempotency-Key", "native-accounting")
				c.Request.Header.Set(mc.HeaderMode, "managed")
				_, err := f.service.RunContext(c, f.keys.key, keys, body, "responses", "", 0, 0)
				return err
			}
			require.Error(t, request())
			var attempts []mc.Attempt
			for _, record := range store.records {
				if record.Kind == "attempt" {
					var a mc.Attempt
					require.NoError(t, json.Unmarshal(record.Data, &a))
					attempts = append(attempts, a)
				}
			}
			require.Len(t, attempts, 1)
			a := attempts[0]
			require.NotNil(t, a.UsageFinal)
			require.Equal(t, final, *a.UsageFinal)
			require.Equal(t, final, a.Settled)
			require.Equal(t, "in_progress", a.ResponseStatus)
			if final {
				require.Len(t, f.bills.commands, 1)
				require.Zero(t, f.cache.count())
			} else {
				require.Equal(t, "unknown", a.State)
				require.Empty(t, f.bills.commands)
				require.Equal(t, 1, f.cache.count())
			}
			require.Error(t, request())
			require.EqualValues(t, 1, f.calls.Load())
		})
	}
}
