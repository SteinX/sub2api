package service

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	mc "github.com/Wei-Shaw/sub2api/internal/pkg/managedcontext"
	"go.uber.org/zap"
)

// ContextMaintenance only reconciles durable attempts and expires content. It
// has no path to dispatch inference or a prepared summary after a restart.
type ContextMaintenance struct {
	stop   context.CancelFunc
	done   chan struct{}
	once   sync.Once
	engine *mc.Engine
}

func NewContextMaintenance(gateway *OpenAIGatewayService, devin *DevinService, keys *APIKeyService) *ContextMaintenance {
	ctx, cancel := context.WithCancel(context.Background())
	w := &ContextMaintenance{stop: cancel, done: make(chan struct{})}
	enabled := gateway != nil && gateway.cfg != nil && keys != nil && devin != nil && devin.contextStore != nil
	if !enabled {
		close(w.done)
		return w
	}
	w.engine = devin.managedEngine()
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				contextMaintenancePass(ctx, gateway, devin, keys)
			}
		}
	}()
	return w
}
func (w *ContextMaintenance) Stop() {
	if w == nil {
		return
	}
	w.once.Do(w.stop)
	<-w.done
	if w.engine != nil {
		ctx, stop := context.WithTimeout(context.Background(), 50*time.Second)
		defer stop()
		if err := w.engine.Close(ctx); err != nil {
			logger.FromContext(ctx).Warn("devin.context_drain", zap.Error(err))
		}
	}
}
func contextMaintenancePass(ctx context.Context, s *OpenAIGatewayService, d *DevinService, keys *APIKeyService) {
	ctx, stop := context.WithTimeout(ctx, 20*time.Second)
	defer stop()
	engine := d.managedEngine()
	if engine == nil {
		return
	}
	if metrics := engine.Metrics(); len(metrics) > 1 {
		logger.FromContext(ctx).Info("devin.context_counters", zap.Any("counters", metrics))
	}
	pending, err := engine.Store.Pending(ctx, 50)
	if err != nil {
		logger.FromContext(ctx).Warn("devin.context_recovery_scan", zap.Error(err))
		return
	}
	for _, record := range pending {
		if ctx.Err() != nil {
			return
		}
		var a mc.Attempt
		if json.Unmarshal(record.Data, &a) != nil {
			continue
		}
		if a.State != "terminal" {
			_ = engine.ReconcileAttempt(ctx, a.Scope, a.ID, nil)
			continue
		}
		// Move persistent failures to the back of the bounded scan, so one
		// unavailable billing owner cannot starve newer observed usage.
		_ = engine.Store.CAS(ctx, record, nil, nil, nil)
		key, err := keys.GetByID(ctx, a.KeyID)
		if err != nil || key == nil || key.UserID != a.UserID {
			logger.FromContext(ctx).Warn("devin.context_settlement_owner_unavailable", zap.String("attempt_id", a.ID))
			continue
		}
		if key.User == nil {
			key.User, err = s.userRepo.GetByID(ctx, a.UserID)
			if err != nil {
				continue
			}
		}
		x, err := s.contextPhysical(ctx, key, keys, a.Owner, a.Model, a.SessionID, mc.DefaultPolicy(), nil, false)
		if err != nil {
			logger.FromContext(ctx).Warn("devin.context_settlement_account_unavailable", zap.String("attempt_id", a.ID), zap.Error(err))
			continue
		}
		err = engine.ReconcileAttempt(ctx, a.Scope, a.ID, x)
		x.physical.Close()
		if err != nil {
			logger.FromContext(ctx).Warn("devin.context_settlement_pending", zap.String("attempt_id", a.ID), zap.Error(err))
		}
	}
	if _, err = engine.Store.Cleanup(ctx, 200); err != nil {
		logger.FromContext(ctx).Warn("devin.context_cleanup", zap.Error(err))
	}
}
