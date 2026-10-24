//go:build unit

package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fusionRecoveryAccountRepo struct {
	*fusionAccountRepo
	errors, limited int
}

func (r *fusionRecoveryAccountRepo) SetError(context.Context, int64, string) error {
	r.errors++
	return nil
}
func (r *fusionRecoveryAccountRepo) SetRateLimited(context.Context, int64, time.Time) error {
	r.limited++
	return nil
}

func fusionRecoveryGateway(t *testing.T, f *warmFixture) (*fusionRecoveryAccountRepo, func() (*httptest.ResponseRecorder, error)) {
	t.Helper()
	repo := &fusionRecoveryAccountRepo{fusionAccountRepo: &fusionAccountRepo{account: f.account}}
	f.service.accountRepo = repo
	f.service.devin.repo = repo
	t.Cleanup(func() { _ = f.service.CloseDevinCacheWarm(context.Background()) })
	return repo, func() (*httptest.ResponseRecorder, error) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		c.Request.Header.Set(fusion.HeaderSession, "recovery-session")
		c.Request.Header.Set("Idempotency-Key", "first-request")
		c.Set("api_key", f.keys.key)
		_, err := f.service.forwardDevin(c.Request.Context(), c, f.account, fusionProtocolBody("responses", false, ""), "responses", "", "")
		return w, err
	}
}

func fusionRecoveryToolReply(model string, successful int) *http.Response {
	name, id, args := fusion.DelegateTool, "delegation", `{"task":"inspect file","acceptance_criteria":"read file evidence"}`
	if successful > 1 {
		name, id, args = "read_file", "read", `{"path":"main.go"}`
	}
	return warmReply(&pb.GetChatMessageResponse{MessageId: fmt.Sprint(successful), StopReason: pb.StopReason_STOP_REASON_FUNCTION_CALL, DeltaToolCalls: []*pb.ChatToolCall{{Id: id, Name: name, ArgumentsJson: args}}, Usage: &pb.ModelUsageStats{InputTokens: 10, OutputTokens: 3, BillingModelUid: model}})
}

func TestFusionGatewayRecoversAuthenticationWithDurableAttempts(t *testing.T) {
	for _, phase := range []string{"prepare", "chat"} {
		t.Run(phase, func(t *testing.T) {
			f := newWarmFixture(t)
			f.account.Credentials["api_key"] = "cog_synthetic"
			key := f.service.devin.runtimeKey(context.Background(), f.account)
			f.service.devin.runtime[key] = devinRuntime{token: "old-token", tokenUntil: time.Now().Add(time.Hour)}
			repo, request := fusionRecoveryGateway(t, f)
			upstream := f.service.devin.upstream.(*devinHTTP)
			original := upstream.handle
			mints, refused, successful := 0, false, 0
			upstream.handle = func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == devin.MintPath {
					mints++
					return devinReply(200, []byte(`{"sessionToken":"fresh-token"}`)), nil
				}
				if phase == "prepare" && r.URL.Path == devin.UserJWTPath && !refused {
					refused = true
					return devinReply(401, []byte(`{"code":"unauthenticated"}`)), nil
				}
				return original(r)
			}
			f.chat = func(r *http.Request, wire *pb.GetChatMessageRequest) (*http.Response, error) {
				f.cache.mu.Lock()
				pending := len(f.cache.res)
				f.cache.mu.Unlock()
				assert.Equal(t, 1, pending, "rejected attempt releases its hold before the replacement reserves")
				if phase == "chat" && !refused {
					refused = true
					return devinReply(401, []byte(`{"code":"unauthenticated"}`)), nil
				}
				user, _, ok := r.BasicAuth()
				assert.True(t, ok)
				assert.Equal(t, "fresh-token", user)
				successful++
				return fusionRecoveryToolReply(wire.ChatModelUid, successful), nil
			}
			w, err := request()
			require.NoError(t, err, w.Body.String())
			require.Equal(t, 200, w.Code)
			require.Equal(t, 1, mints)
			require.Zero(t, repo.errors, "a recovered session is not an invalid account")
			require.Len(t, f.bills.commands, 2, "only successful physical inference is billed")
			session, _, err := f.engine.Load(context.Background(), fusion.Scope(f.keys.key.UserID, f.keys.key.ID), "recovery-session")
			require.NoError(t, err)
			require.Len(t, session.Attempts, 3)
			require.Equal(t, session.Attempts[0].ID, session.Attempts[1].RetryOf)
			require.Equal(t, fusion.RejectionAuth, session.Attempts[1].RecoveryKind)
			require.True(t, session.Attempts[0].Settled)
			before := f.calls.Load()
			w, err = request()
			require.NoError(t, err, w.Body.String())
			require.Equal(t, before, f.calls.Load(), "retry replays the durable public response")
			require.Len(t, f.bills.commands, 2)
		})
	}
}

func TestFusionGatewayExplicitRejectionsAreBounded(t *testing.T) {
	for _, kind := range []string{"rate", "capacity"} {
		t.Run(kind, func(t *testing.T) {
			f := newWarmFixture(t)
			repo, request := fusionRecoveryGateway(t, f)
			f.chat = func(_ *http.Request, _ *pb.GetChatMessageRequest) (*http.Response, error) {
				if kind == "rate" {
					r := devinReply(429, []byte(`{"code":"resource_exhausted"}`))
					r.Header.Set("Retry-After", "0")
					return r, nil
				}
				body := []byte(`{"error":{"code":"unimplemented","message":"We are currently experiencing capacity issues with this serving model"},"metadata":{"Retry-After":["0"]}}`)
				return devinReply(200, devinTestFrame(2, body)), nil
			}
			_, err := request()
			require.Error(t, err)
			require.EqualValues(t, 3, f.calls.Load(), "one initial generation plus two extra sends")
			require.Empty(t, f.bills.commands)
			require.Zero(t, repo.errors)
			if kind == "capacity" {
				require.Zero(t, repo.limited, "model capacity is not an account penalty")
			}
			_, _ = request()
			require.EqualValues(t, 3, f.calls.Load(), "HTTP retries do not reset the persisted send limit")
		})
	}
}

func TestFusionGatewayFailedAuthenticationRecoveryStops(t *testing.T) {
	f := newWarmFixture(t)
	f.account.Credentials["api_key"] = "cog_synthetic"
	key := f.service.devin.runtimeKey(context.Background(), f.account)
	f.service.devin.runtime[key] = devinRuntime{token: "old-token", tokenUntil: time.Now().Add(time.Hour)}
	repo, request := fusionRecoveryGateway(t, f)
	upstream := f.service.devin.upstream.(*devinHTTP)
	original := upstream.handle
	mints := 0
	upstream.handle = func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == devin.MintPath {
			mints++
			return devinReply(200, []byte(`{"sessionToken":"fresh-token"}`)), nil
		}
		return original(r)
	}
	f.chat = func(_ *http.Request, _ *pb.GetChatMessageRequest) (*http.Response, error) {
		return devinReply(401, []byte(`{"code":"unauthenticated"}`)), nil
	}
	_, err := request()
	require.Error(t, err)
	require.EqualValues(t, 2, f.calls.Load())
	require.Equal(t, 1, mints)
	require.Equal(t, 1, repo.errors, "only the failed refreshed credential marks the account invalid")
	require.Empty(t, f.bills.commands)
	_, _ = request()
	require.Equal(t, 1, mints)
	require.EqualValues(t, 2, f.calls.Load())
}

func TestFusionGatewayRechecksAdmissionAfterPrepare(t *testing.T) {
	f := newWarmFixture(t)
	_, request := fusionRecoveryGateway(t, f)
	upstream := f.service.devin.upstream.(*devinHTTP)
	original := upstream.handle
	blocked := false
	upstream.handle = func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == devin.UserJWTPath && !blocked {
			blocked = true
			// Simulate a concurrent worker discovering the account cooldown
			// after this request's first admission check but before Chat.
			if err := f.service.devin.noteFusionRejection(r.Context(), f.account, f.models[0].ModelUid, &fusion.Rejection{Kind: fusion.RejectionRateLimit, NotExecuted: true, RetryAfter: time.Hour}); err != nil {
				return nil, err
			}
		}
		return original(r)
	}
	_, err := request()
	require.Error(t, err)
	require.True(t, blocked)
	require.Zero(t, f.calls.Load(), "the newly shared cooldown prevents the generation send")
	require.Empty(t, f.bills.commands)
	f.cache.mu.Lock()
	pending := len(f.cache.res)
	f.cache.mu.Unlock()
	require.Zero(t, pending, "a pre-send rejection releases its monetary reservation")
}

func TestFusionGatewayDoesNotReissueAmbiguousGeneration(t *testing.T) {
	for _, failure := range []string{"eof", "server-error", "partial-auth", "usage-auth", "tokens-auth", "credit-auth", "committed-credit-auth", "acu-auth", "redacted-auth", "stop-auth"} {
		t.Run(failure, func(t *testing.T) {
			f := newWarmFixture(t)
			_, request := fusionRecoveryGateway(t, f)
			f.chat = func(_ *http.Request, wire *pb.GetChatMessageRequest) (*http.Response, error) {
				switch failure {
				case "eof":
					return nil, io.ErrUnexpectedEOF
				case "server-error":
					return devinReply(503, []byte(`{"code":"unauthenticated"}`)), nil
				default:
					message := &pb.GetChatMessageResponse{DeltaText: "partial"}
					switch failure {
					case "usage-auth":
						message = &pb.GetChatMessageResponse{Usage: &pb.ModelUsageStats{InputTokens: 5, OutputTokens: 1, BillingModelUid: wire.ChatModelUid}}
					case "tokens-auth":
						message = &pb.GetChatMessageResponse{DeltaTokens: 1}
					case "credit-auth":
						message = &pb.GetChatMessageResponse{CreditCost: 1}
					case "committed-credit-auth":
						message = &pb.GetChatMessageResponse{CommittedCreditCost: 1}
					case "acu-auth":
						message = &pb.GetChatMessageResponse{CommittedAcuCost: 1}
					case "redacted-auth":
						message = &pb.GetChatMessageResponse{ThinkingRedacted: true}
					case "stop-auth":
						message = &pb.GetChatMessageResponse{StopReason: pb.StopReason_STOP_REASON_MAX_TOKENS}
					}
					r := warmReply(message)
					body, err := io.ReadAll(r.Body)
					assert.NoError(t, err)
					body = body[:len(body)-len(devinTestFrame(2, []byte(`{}`)))]
					return devinReply(200, append(body, devinTestFrame(2, []byte(`{"error":{"code":"unauthenticated"}}`))...)), nil
				}
			}
			_, err := request()
			require.Error(t, err)
			require.EqualValues(t, 1, f.calls.Load())
			_, _ = request()
			require.EqualValues(t, 1, f.calls.Load(), "ambiguous or already billed inference cannot be reissued")
			if failure == "usage-auth" {
				require.Len(t, f.bills.commands, 1)
			} else {
				require.Empty(t, f.bills.commands)
			}
		})
	}
}

func TestFusionWarmNativeExecutionEvidenceRetainsUnknownReservation(t *testing.T) {
	for _, fixture := range []struct {
		name    string
		message *pb.GetChatMessageResponse
	}{
		{"tokens", &pb.GetChatMessageResponse{DeltaTokens: 1}},
		{"committed_cost", &pb.GetChatMessageResponse{CommittedAcuCost: 1}},
		{"redacted_thinking", &pb.GetChatMessageResponse{ThinkingRedacted: true}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			f := newWarmFixture(t)
			f.chat = func(_ *http.Request, _ *pb.GetChatMessageRequest) (*http.Response, error) {
				r := warmReply(fixture.message)
				body, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				body = body[:len(body)-len(devinTestFrame(2, []byte(`{}`)))]
				return devinReply(200, append(body, devinTestFrame(2, []byte(`{"error":{"code":"resource_exhausted"}}`))...)), nil
			}
			result, err := f.adapter.Refresh(context.Background(), f.snapshot, fusion.Attempt{ID: "executed-warm", Purpose: fusion.CacheRefresh, Role: fusion.Lead, EstimateUSD: 1, StartedAt: time.Now()})
			require.Error(t, err)
			require.False(t, result.Outcome.KnownFailure, "native evidence prevents treating the inference as an unexecuted refusal")
			require.EqualValues(t, 1, f.calls.Load())
			require.NoError(t, f.service.devin.waitFusionAdmission(context.Background(), f.account, f.snapshot.Model, fusion.CacheRefresh), "an ambiguous terminal error does not install a refusal cooldown")
			f.cache.mu.Lock()
			pending := len(f.cache.res)
			f.cache.mu.Unlock()
			require.Equal(t, 1, pending, "unknown execution retains the monetary reservation")
			require.Empty(t, f.bills.commands)
		})
	}
}

func TestFusionExecutorPreservesManagedContextAuthentication(t *testing.T) {
	for _, phase := range []string{"prepare", "chat"} {
		t.Run(phase, func(t *testing.T) {
			f := newWarmFixture(t)
			repo, _ := fusionRecoveryGateway(t, f)
			// Managed context uses its SQL journal and does not require a Fusion
			// session store, even though the physical transport is shared.
			f.service.devin.store = nil
			f.account.Credentials["api_key"] = "cog_synthetic"
			key := f.service.devin.runtimeKey(context.Background(), f.account)
			f.service.devin.runtime[key] = devinRuntime{token: "old-token", tokenUntil: time.Now().Add(time.Hour)}
			upstream := f.service.devin.upstream.(*devinHTTP)
			original := upstream.handle
			mints, refused := 0, false
			upstream.handle = func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == devin.MintPath {
					mints++
					return devinReply(200, []byte(`{"sessionToken":"fresh-token"}`)), nil
				}
				if phase == "prepare" && r.URL.Path == devin.UserJWTPath && !refused {
					refused = true
					return devinReply(401, []byte(`{"code":"unauthenticated"}`)), nil
				}
				return original(r)
			}
			f.chat = func(r *http.Request, wire *pb.GetChatMessageRequest) (*http.Response, error) {
				if phase == "chat" {
					return devinReply(401, []byte(`{"code":"unauthenticated"}`)), nil
				}
				user, _, ok := r.BasicAuth()
				assert.True(t, ok)
				assert.Equal(t, "fresh-token", user)
				return warmReply(&pb.GetChatMessageResponse{DeltaText: "context complete", StopReason: pb.StopReason_STOP_REASON_STOP_PATTERN, Usage: &pb.ModelUsageStats{InputTokens: 4, OutputTokens: 1, BillingModelUid: wire.ChatModelUid}}), nil
			}
			client, err := f.service.devin.Client(context.Background(), f.account)
			require.NoError(t, err)
			model := f.models[0].ModelUid
			profile := f.session.Profile
			profile.PublicModel = model
			x := &devinFusionExecutor{attemptPrefix: "context", service: f.service, account: f.account, key: f.keys.key, profile: profile, publicModel: model, client: client, models: f.models, reservations: map[string]*InflightReservation{}}
			defer x.Close()
			limit := 256
			call := fusion.Call{AttemptID: "context-auth", Role: fusion.Role("main"), Purpose: fusion.Inference, Request: &apicompat.ResponsesRequest{Model: model, Input: []byte(`"task"`), MaxOutputTokens: &limit}, LaneScope: "context-auth-lane", PricingAt: time.Now(), EstimateUSD: .01}
			outcome, err := x.Execute(context.Background(), call)
			require.EqualValues(t, 1, f.calls.Load(), "preparation recovery must not replay a physical generation")
			if phase == "chat" {
				require.Error(t, err)
				require.True(t, outcome.KnownFailure)
				require.Nil(t, outcome.Rejection, "managed context does not consume Fusion recovery metadata")
				require.Zero(t, mints)
				require.Equal(t, 1, repo.errors)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 1, mints)
			require.Zero(t, repo.errors)
			require.NotNil(t, outcome.Response.Usage)
			_, err = x.Settle(context.Background(), fusion.Attempt{ID: call.AttemptID, Model: model, BillingModel: model, StartedAt: call.PricingAt, EstimateUSD: call.EstimateUSD, Response: outcome.Response})
			require.NoError(t, err)
			require.Len(t, f.bills.commands, 1)
		})
	}
}
