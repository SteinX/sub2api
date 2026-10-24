package service

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
)

type fusionRateLimitExtendingRepository interface {
	SetRateLimitedIfLater(context.Context, int64, time.Time) error
}

// recordFusionRejection is only called before generation or after a terminal
// rejection with neither output nor usage. The state machine owns every resend.
func (x *devinFusionExecutor) recordFusionRejection(ctx context.Context, call fusion.Call, err error) (*fusion.Rejection, error) {
	var upstream *devin.Error
	if !errors.As(err, &upstream) || !upstream.RetryableRejection() {
		return nil, nil
	}
	r := &fusion.Rejection{NotExecuted: true}
	switch upstream.RejectionKind {
	case devin.RejectionAuthentication:
		if x.account.GetCredential("api_key") == "" || call.AuthRecoveryUsed || x.attemptPrefix == "context" {
			if x.service.accountRepo != nil {
				cleanup, release := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				defer release()
				_ = x.service.accountRepo.SetError(cleanup, x.account.ID, "Devin authorization expired; reconnect this account")
			}
			return nil, nil
		}
		r.Kind = fusion.RejectionAuth
	case devin.RejectionRateLimit:
		r.Kind, r.RetryAfter = fusion.RejectionRateLimit, time.Minute
	case devin.RejectionModelCapacity:
		r.Kind, r.RetryAfter = fusion.RejectionModelCapacity, 2*time.Second
	default:
		return nil, nil
	}
	if upstream.HasRetryAfter && r.Kind != fusion.RejectionAuth {
		r.RetryAfter = upstream.RetryAfter
	}
	cleanup, release := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer release()
	// The SQL-backed managed-context engine does not use Fusion's Redis
	// recovery journal or admission gate, even though it shares the executor.
	if x.attemptPrefix != "context" {
		if err := x.service.devin.noteFusionRejection(cleanup, x.account, call.Request.Model, r); err != nil {
			return nil, err
		}
	}
	if r.Kind == fusion.RejectionRateLimit {
		// Redis is the shared Fusion gate. Mirror it to scheduling only when
		// the repository can atomically preserve a longer existing SQL reset;
		// legacy setters cannot safely emulate that with a read/write pair.
		if extender, ok := x.service.accountRepo.(fusionRateLimitExtendingRepository); ok {
			_ = extender.SetRateLimitedIfLater(cleanup, x.account.ID, time.Now().Add(r.RetryAfter))
		}
	}
	return r, nil
}
