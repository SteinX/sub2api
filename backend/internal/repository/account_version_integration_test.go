//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestAccountRowVersionUsesMutationOrderInsteadOfTransactionStart(t *testing.T) {
	ctx := context.Background()
	account, err := testEntClient(t).Account.Create().
		SetName("cache-row-version").SetPlatform(service.PlatformDevin).
		SetType(service.AccountTypeAPIKey).Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = testEntClient(t).Account.DeleteOneID(account.ID).Exec(ctx) })

	older, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer older.Rollback() //nolint:errcheck
	var oldTransactionTime time.Time
	require.NoError(t, older.QueryRowContext(ctx, "SELECT transaction_timestamp()").Scan(&oldTransactionTime))
	var newer time.Time
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"UPDATE accounts SET name = 'newer transaction' WHERE id=$1 RETURNING updated_at", account.ID).Scan(&newer))

	// The older transaction mutates last. An ordinary NOW() assignment would
	// backdate this row even though this is now the authoritative DB value.
	var last, sameTransactionNext time.Time
	require.NoError(t, older.QueryRowContext(ctx,
		"UPDATE accounts SET name = 'older transaction commits last', updated_at=transaction_timestamp() WHERE id=$1 RETURNING updated_at", account.ID).Scan(&last))
	require.True(t, last.After(newer), "latest committed row must have the newer version")
	require.NoError(t, older.QueryRowContext(ctx,
		"UPDATE accounts SET updated_at=transaction_timestamp() WHERE id=$1 RETURNING updated_at", account.ID).Scan(&sameTransactionNext))
	require.True(t, sameTransactionNext.After(last), "multiple mutations in one transaction need distinct versions")
	require.GreaterOrEqual(t, sameTransactionNext.Sub(last), time.Microsecond)
	require.NoError(t, older.Commit())
}
