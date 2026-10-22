package service

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
)

// Usage shares credential/proxy-scoped caches and token recovery with chat and
// model discovery. Failed probes are cached briefly without changing scheduling.
func (s *DevinService) Usage(ctx context.Context, account *Account, force bool) (*UsageInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	key := s.runtimeKey(account)
	cached := func() *UsageInfo {
		s.mu.Lock()
		defer s.mu.Unlock()
		entry := s.runtime[key]
		if !force && time.Now().Before(entry.usageUntil) {
			return entry.usage
		}
		return nil
	}
	if usage := cached(); usage != nil {
		return usage, nil
	}
	pending := s.flights.DoChan("usage:"+key, func() (any, error) {
		if usage := cached(); usage != nil {
			return usage, nil
		}
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		usage := s.fetchUsage(probeCtx, account)
		ttl := apiCacheTTL
		if usage.ErrorCode != "" {
			ttl = apiErrorCacheTTL
		}
		now := time.Now()
		until := now.Add(ttl)
		for _, window := range []*UsageProgress{usage.Daily, usage.SevenDay} {
			if window != nil && window.ResetsAt != nil && window.ResetsAt.After(now) && window.ResetsAt.Before(until) {
				until = *window.ResetsAt
			}
		}
		s.mu.Lock()
		if len(s.runtime) > 2048 {
			for k, entry := range s.runtime {
				if now.After(entry.tokenUntil) && now.After(entry.modelsUntil) && now.After(entry.usageUntil) {
					delete(s.runtime, k)
				}
			}
		}
		entry := s.runtime[key]
		entry.usage, entry.usageUntil = usage, until
		s.runtime[key] = entry
		s.mu.Unlock()
		return usage, nil
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-pending:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if result.Err != nil {
			return nil, result.Err
		}
		return result.Val.(*UsageInfo), nil
	}
}

func (s *DevinService) fetchUsage(ctx context.Context, account *Account) *UsageInfo {
	token, err := s.Token(ctx, account, false)
	if err != nil {
		return devinUsageError(err)
	}
	client, err := s.Client(ctx, account)
	if err != nil {
		return devinUsageError(err)
	}
	status, err := client.Usage(ctx, token)
	if devin.IsUnauthenticated(err) && account.GetCredential("api_key") != "" {
		token, err = s.RecoverToken(ctx, account, token)
		if err == nil {
			status, err = client.Usage(ctx, token)
		}
	}
	if err != nil {
		return devinUsageError(err)
	}
	now := time.Now()
	return &UsageInfo{
		Source: "active", UpdatedAt: &now, DevinUsage: status,
		Daily: devinUsageProgress(status.Daily, now), SevenDay: devinUsageProgress(status.Weekly, now),
	}
}

func devinUsageProgress(window *devin.QuotaWindow, now time.Time) *UsageProgress {
	// A missing/null quota is unknown. It must not become a fabricated 0% or
	// 100% utilization through a scalar's zero value.
	if window == nil || window.RemainingPercent == nil {
		return nil
	}
	progress := &UsageProgress{Utilization: 100 - *window.RemainingPercent, ResetsAt: window.ResetsAt}
	if window.ResetsAt != nil {
		progress.RemainingSeconds = max(0, int(window.ResetsAt.Sub(now).Seconds()))
	}
	return progress
}

func devinUsageError(err error) *UsageInfo {
	info := &UsageInfo{Source: "active", ErrorCode: errorCodeNetworkError, Error: "Devin usage query failed"}
	var upstreamErr *devin.Error
	if errors.As(err, &upstreamErr) {
		switch upstreamErr.HTTPStatus() {
		case 401:
			info.ErrorCode, info.NeedsReauth = errorCodeUnauthenticated, true
			info.Error = "Devin authorization expired"
		case 403:
			info.ErrorCode = errorCodeForbidden
			info.Error = "Devin usage is unavailable for this account or plan"
		case 429:
			info.ErrorCode = errorCodeRateLimited
			info.Error = "Devin usage query rate limited"
		}
	}
	return info
}
