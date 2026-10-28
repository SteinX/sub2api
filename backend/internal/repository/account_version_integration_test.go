//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestAccountRowVersionUpgradeKeepsLegacySQLCompatible(t *testing.T) {
	ctx := context.Background()
	tx, err := integrationDB.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck
	// This is an isolated test transaction, never a deployed database. Model
	// an existing row before the new trigger, then apply the unchanged upgrade.
	_, err = tx.ExecContext(ctx, "DROP TRIGGER accounts_monotonic_updated_at ON accounts")
	require.NoError(t, err)
	var id int64
	var before time.Time
	require.NoError(t, tx.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,updated_at)
		VALUES('pre-upgrade-row','devin','apikey','2000-01-01T00:00:00Z') RETURNING id,updated_at`).Scan(&id, &before))
	upgrade, err := migrations.FS.ReadFile("248_monotonic_account_updated_at.sql")
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, string(upgrade))
	require.NoError(t, err)
	var after, legacyWriter time.Time
	require.NoError(t, tx.QueryRowContext(ctx,
		"UPDATE accounts SET name='after upgrade' WHERE id=$1 RETURNING updated_at", id).Scan(&after))
	require.True(t, after.After(before))
	// The old binary's field shape and SQL remain valid if code is rolled
	// back. The forward-only trigger remains; old cache correctness is not
	// claimed by this schema compatibility check.
	require.NoError(t, tx.QueryRowContext(ctx,
		"UPDATE accounts SET credentials='{}'::jsonb, updated_at=$2 WHERE id=$1 RETURNING updated_at", id, before).Scan(&legacyWriter))
	require.True(t, legacyWriter.After(after))
}

func TestAccountRowVersionDelayedCachePublicationWithPostgresAndRedis(t *testing.T) {
	ctx := context.Background()
	model, err := testEntClient(t).Account.Create().SetName("old row").
		SetPlatform(service.PlatformDevin).SetType(service.AccountTypeAPIKey).Save(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = testEntClient(t).Account.DeleteOneID(model.ID).Exec(ctx) })
	cache := NewSchedulerCache(testRedis(t))
	bucket := service.SchedulerBucket{GroupID: 18, Platform: service.PlatformDevin, Mode: service.SchedulerModeSingle}
	old := service.Account{ID: model.ID, Name: model.Name, UpdatedAt: model.UpdatedAt, Platform: service.PlatformDevin, GroupIDs: []int64{18}}
	require.NoError(t, integrationDB.QueryRowContext(ctx, "SELECT updated_at FROM accounts WHERE id=$1", old.ID).Scan(&old.UpdatedAt))
	token, err := cache.CaptureBucketWriteToken(ctx, bucket)
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{old}))
	_, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	newer := old
	require.NoError(t, integrationDB.QueryRowContext(ctx,
		"UPDATE accounts SET name='new row' WHERE id=$1 RETURNING name,updated_at", old.ID).Scan(&newer.Name, &newer.UpdatedAt))
	require.True(t, newer.UpdatedAt.After(old.UpdatedAt))
	require.NoError(t, cache.SetAccount(ctx, &newer))
	require.ErrorIs(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{old}), service.ErrSchedulerBucketWriteFenced)
	actual, err := cache.GetAccount(ctx, old.ID)
	require.NoError(t, err)
	require.Equal(t, "new row", actual.Name)
	metadata, hit, err := cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.True(t, hit)
	require.Equal(t, "new row", metadata[0].Name)
	require.NoError(t, cache.DeleteAccount(ctx, old.ID))
	require.ErrorIs(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{newer}), service.ErrSchedulerBucketWriteFenced)
	_, hit, err = cache.GetSnapshot(ctx, bucket)
	require.NoError(t, err)
	require.False(t, hit)
}

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
