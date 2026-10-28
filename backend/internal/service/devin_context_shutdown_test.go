//go:build unit

package service

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	mc "github.com/Wei-Shaw/sub2api/internal/pkg/managedcontext"
	"github.com/stretchr/testify/require"
)

func TestContextMaintenanceShutdownWaitsForLatePersistence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		engine := mc.New(newContextGatewayStore())
		op, _, err := engine.Begin(context.Background(), mc.Owner{Scope: "scope", UserID: 1, KeyID: 2}, "stateless", "shutdown", "input", "model", "", mc.DefaultPolicy())
		require.NoError(t, err)
		started := make(chan struct{})
		var persisted atomic.Bool
		finished := make(chan error, 1)
		go func() {
			finished <- engine.WithOperation(context.Background(), op, func(ctx context.Context, _ *mc.Operation, _ *mc.Record, _ *mc.Lease) error {
				close(started)
				<-ctx.Done()
				// Virtual time: this is inside the real executor's 90s late
				// response window, after the old 50s shutdown cutoff.
				time.Sleep(75 * time.Second)
				persisted.Store(true)
				return nil
			})
		}()
		<-started
		done := make(chan struct{})
		close(done)
		w := &ContextMaintenance{stop: func() {}, done: done, engine: engine}
		w.Stop()
		require.True(t, persisted.Load(), "dependency cleanup must follow late usage persistence")
		require.NoError(t, <-finished)
	})
}
