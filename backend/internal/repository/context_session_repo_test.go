//go:build integration || contextintegration

package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	mc "github.com/Wei-Shaw/sub2api/internal/pkg/managedcontext"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/require"
)

// contextintegration can run against an explicit local DSN. The ordinary
// integration suite supplies its testcontainers DSN through the shared harness.
var contextTestDatabaseDSN string

func contextTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("CONTEXT_TEST_DATABASE_URL")
	if dsn == "" {
		dsn = contextTestDatabaseDSN
	}
	if dsn == "" {
		t.Skip("set CONTEXT_TEST_DATABASE_URL to an isolated PostgreSQL test database")
	}
	admin, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	schema := "context_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = admin.Exec(`CREATE SCHEMA ` + schema)
	require.NoError(t, err)
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("postgres", dsn+sep+"search_path="+schema)
	require.NoError(t, err)
	db.SetMaxOpenConns(12)
	t.Cleanup(func() { _ = db.Close(); _, _ = admin.Exec(`DROP SCHEMA ` + schema + ` CASCADE`); _ = admin.Close() })
	migration, err := os.ReadFile("../../migrations/247_context_sessions.sql")
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err)
	_, err = db.Exec(string(migration))
	require.NoError(t, err, "migration is idempotent")
	return db
}
func contextRecord(kind, id string, user, key int64) *mc.Record {
	return &mc.Record{Owner: mc.Owner{Scope: "scope", UserID: user, KeyID: key, AccountID: 3}, Kind: kind, ID: id, Status: "ready", Data: json.RawMessage(`{"history_bytes":0,"reserved_bytes":0}`), ExpiresAt: time.Now().Add(time.Hour)}
}
func contextBlob(id, session, visibility string, data []byte) mc.Blob {
	return mc.Blob{ID: id, SessionID: session, Visibility: visibility, Kind: "message", Data: data, ExpiresAt: time.Now().Add(time.Hour)}
}

func TestContextStorePostgresReservationsSurvivePhysicalWrites(t *testing.T) {
	for _, mode := range []string{"session", "stateless", "manual", "legacy_manual_job"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			db := contextTestDB(t)
			s := NewContextSessionStore(db)
			p := mc.DefaultPolicy()
			p.MaxHistoryBytes, p.MaxUserHistoryBytes = 1<<20, 1500<<10
			a := contextRecord("session", "a", 1, 2)
			a.Data = json.RawMessage(`{"operation_id":"op-a","reserved_bytes":921600}`)
			_, err := s.Create(ctx, a, &p)
			require.NoError(t, err)
			la, err := s.Acquire(ctx, a.Scope, a.Kind, a.ID, "owner-a", time.Minute)
			require.NoError(t, err)
			require.NoError(t, s.CAS(ctx, a, &la, nil, &p))
			b := contextRecord("session", "b", 1, 3)
			b.Data = json.RawMessage(`{"operation_id":"op-b","reserved_bytes":512000}`)
			bSession := "b"
			switch mode {
			case "stateless":
				b.Kind, b.ID, bSession = "operation", "op-b", ""
			case "manual":
				b.Data = json.RawMessage(`{"reservation_operation_id":"op-b","reserved_bytes":512000}`)
			case "legacy_manual_job":
				b.Data = json.RawMessage(`{"job_id":"manual-job","reserved_bytes":512000}`)
				job := contextRecord("job", "manual-job", 1, 3)
				job.Data = json.RawMessage(`{"operation_id":"op-b"}`)
				_, err = s.Create(ctx, job, nil)
				require.NoError(t, err)
			}
			_, err = s.Create(ctx, b, &p)
			require.NoError(t, err)
			lb, err := s.Acquire(ctx, b.Scope, b.Kind, b.ID, "owner-b", time.Minute)
			require.NoError(t, err)
			require.NoError(t, s.CAS(ctx, b, &lb, nil, &p))
			attemptB := contextRecord("attempt", "attempt-b", 1, 3)
			attemptB.Data, _ = json.Marshal(map[string]any{"operation_id": "op-b", "session_id": bSession, "response_ref": "result-b"})
			_, err = s.Create(ctx, attemptB, nil)
			require.NoError(t, err)
			err = s.CAS(ctx, attemptB, nil, []mc.Blob{contextBlob("result-b", bSession, "private", bytes.Repeat([]byte("b"), 800<<10))}, &p)
			require.Error(t, err, "oversized result cannot spend another turn's reservation")
			_, err = s.Blob(ctx, attemptB.Scope, "result-b")
			require.ErrorIs(t, err, mc.ErrNotFound, "rejected result is rolled back")
			attemptA := contextRecord("attempt", "attempt-a", 1, 2)
			attemptA.Data = json.RawMessage(`{"operation_id":"op-a","session_id":"a","response_ref":"result-a"}`)
			_, err = s.Create(ctx, attemptA, nil)
			require.NoError(t, err)
			require.NoError(t, s.CAS(ctx, attemptA, nil, []mc.Blob{contextBlob("result-a", "a", "private", bytes.Repeat([]byte("a"), 800<<10))}, &p))
			require.NoError(t, s.CAS(ctx, a, &la, nil, &p), "persisted physical bytes are credited against their own promise")
			require.NoError(t, s.CAS(ctx, attemptB, nil, []mc.Blob{contextBlob("result-b", bSession, "private", bytes.Repeat([]byte("b"), 400<<10))}, &p))
			if mode == "legacy_manual_job" {
				b.Data = json.RawMessage(`{"applied_job_id":"manual-job","reserved_bytes":512000}`)
				require.NoError(t, s.CAS(ctx, b, &lb, nil, &p), "credit survives application clearing the active job marker")
			}
			a.Data = json.RawMessage(`{"reserved_bytes":0}`)
			require.NoError(t, s.CAS(ctx, a, &la, []mc.Blob{contextBlob("public-a", "a", "public", bytes.Repeat([]byte("a"), 100<<10))}, &p))
			b.Data = json.RawMessage(`{"reserved_bytes":0}`)
			require.NoError(t, s.CAS(ctx, b, &lb, []mc.Blob{contextBlob("public-b", bSession, "public", bytes.Repeat([]byte("b"), 100<<10))}, &p))
		})
	}
}

func TestContextStorePostgresRetainsUnsettledBodiesAndRecoveryWindow(t *testing.T) {
	ctx := context.Background()
	db := contextTestDB(t)
	s := NewContextSessionStore(db)
	for _, r := range []*mc.Record{contextRecord("session", "s", 1, 2), contextRecord("operation", "op", 1, 2)} {
		_, err := s.Create(ctx, r, nil)
		require.NoError(t, err)
	}
	a := contextRecord("attempt", "late", 1, 2)
	a.Status = "terminal"
	a.Data = json.RawMessage(`{"session_id":"s","operation_id":"op","response_ref":"result-late","settled":false}`)
	_, err := s.Create(ctx, a, nil)
	require.NoError(t, err)
	require.NoError(t, s.CAS(ctx, a, nil, []mc.Blob{contextBlob("result-late", "s", "private", []byte("observed provider result")), contextBlob("source", "s", "public", []byte("original input"))}, nil))
	_, err = db.Exec(`UPDATE context_history SET expires_at=NOW()-interval '1 day'`)
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE context_records SET expires_at=NOW()-interval '1 day',lease_until=NULL`)
	require.NoError(t, err)
	_, err = s.Cleanup(ctx, 100)
	require.NoError(t, err)
	for _, id := range []string{"result-late", "source"} {
		_, err = s.Blob(ctx, a.Scope, id)
		require.NoError(t, err, "unsettled recovery keeps both response and source history")
	}
	a, err = s.Read(ctx, a.Scope, a.Kind, a.ID)
	require.NoError(t, err)
	a.Data = json.RawMessage(`{"session_id":"s","operation_id":"op","response_ref":"result-late","settled":true}`)
	require.NoError(t, s.CAS(ctx, a, nil, nil, nil))
	_, err = s.Cleanup(ctx, 100)
	require.NoError(t, err)
	_, err = s.Blob(ctx, a.Scope, "result-late")
	require.NoError(t, err, "settlement leaves a recovery window for the paid result")
	_, err = db.Exec(`UPDATE context_records SET updated_at=NOW()-interval '25 hours'`)
	require.NoError(t, err)
	_, err = s.Cleanup(ctx, 100)
	require.NoError(t, err)
	_, err = s.Blob(ctx, a.Scope, "result-late")
	require.ErrorIs(t, err, mc.ErrNotFound)
	_, err = s.Read(ctx, a.Scope, "operation", "op")
	require.ErrorIs(t, err, mc.ErrNotFound)
}

func TestContextStorePostgres(t *testing.T) {
	ctx := context.Background()
	t.Run("quotas_and_scopes", func(t *testing.T) {
		db := contextTestDB(t)
		s := NewContextSessionStore(db)
		p := mc.DefaultPolicy()
		p.MaxSessions = 1
		a := contextRecord("session", "a", 1, 2)
		ok, err := s.Create(ctx, a, &p)
		require.NoError(t, err)
		require.True(t, ok)
		duplicate := contextRecord("session", "a", 1, 2)
		ok, err = s.Create(ctx, duplicate, &p)
		require.NoError(t, err)
		require.False(t, ok)
		_, err = s.Create(ctx, contextRecord("session", "b", 1, 2), &p)
		require.Error(t, err)
		b := contextRecord("session", "b", 1, 9)
		ok, err = s.Create(ctx, b, &p)
		require.NoError(t, err)
		require.True(t, ok)
		p.MaxRequests = 1
		op := contextRecord("operation", "op-a", 1, 2)
		ok, err = s.Create(ctx, op, &p)
		require.NoError(t, err)
		require.True(t, ok)
		ok, err = s.Create(ctx, contextRecord("operation", "op-a", 1, 2), &p)
		require.NoError(t, err)
		require.False(t, ok)
		_, err = s.Create(ctx, contextRecord("operation", "op-b", 1, 2), &p)
		require.Error(t, err, "metadata is quota-bound even without historical blobs")
		_, err = s.Read(ctx, "another-scope", "session", "a")
		require.ErrorIs(t, err, mc.ErrNotFound)
	})
	t.Run("two_owners_fencing_and_cancel", func(t *testing.T) {
		db := contextTestDB(t)
		s := NewContextSessionStore(db)
		other := NewContextSessionStore(db)
		r := contextRecord("session", "s", 1, 2)
		_, err := s.Create(ctx, r, nil)
		require.NoError(t, err)
		var wg sync.WaitGroup
		wins := make(chan mc.Lease, 20)
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l, err := other.Acquire(ctx, r.Scope, r.Kind, r.ID, uuid.NewString(), time.Minute)
				if err == nil {
					wins <- l
				}
			}()
		}
		wg.Wait()
		close(wins)
		require.Len(t, wins, 1)
		old := <-wins
		_, err = db.Exec(`UPDATE context_records SET lease_until=NOW()-interval '1 second' WHERE id='s'`)
		require.NoError(t, err)
		live, err := s.Acquire(ctx, r.Scope, r.Kind, r.ID, "new-owner", time.Minute)
		require.NoError(t, err)
		require.Greater(t, live.Token, old.Token)
		require.Error(t, s.CAS(ctx, r, &old, nil, nil))
		require.NoError(t, s.Release(ctx, old))
		require.NoError(t, s.Renew(ctx, live, time.Minute))
		wrong := live
		wrong.ID = "another"
		require.Error(t, s.CAS(ctx, r, &wrong, nil, nil))
		require.NoError(t, s.CAS(ctx, r, &live, nil, nil))
		stale := *r
		stale.Revision--
		require.Error(t, s.CAS(ctx, &stale, &live, nil, nil))
		require.NoError(t, s.Cancel(ctx, r.Scope, r.ID))
		require.Error(t, s.CAS(ctx, r, &live, nil, nil))
		cancelled, err := s.Read(ctx, r.Scope, r.Kind, r.ID)
		require.NoError(t, err)
		require.True(t, cancelled.Cancelled)
		a := contextRecord("attempt", "late", 1, 2)
		a.Data = json.RawMessage(`{"session_id":"s","settled":false}`)
		_, err = s.Create(ctx, a, nil)
		require.NoError(t, err)
		a.Status = "terminal"
		require.NoError(t, s.CAS(ctx, a, nil, nil, nil), "late terminal evidence survives cancellation")
	})
	t.Run("chunks_visibility_immutable_transaction", func(t *testing.T) {
		db := contextTestDB(t)
		s := NewContextSessionStore(db)
		r := contextRecord("session", "s", 1, 2)
		_, err := s.Create(ctx, r, nil)
		require.NoError(t, err)
		l, err := s.Acquire(ctx, r.Scope, r.Kind, r.ID, "owner", time.Minute)
		require.NoError(t, err)
		data := []byte(`{"text":"` + strings.Repeat("a", (1<<20)+37) + `"}`)
		require.NoError(t, s.CAS(ctx, r, &l, []mc.Blob{contextBlob("large", "s", "public", data), contextBlob("small", "s", "public", []byte(`"hello"`)), contextBlob("private", "s", "private", []byte(`"reasoning"`))}, nil))
		got, err := s.Blob(ctx, r.Scope, "large")
		require.NoError(t, err)
		require.Equal(t, data, got)
		chunk, total, digest, err := s.Chunk(ctx, r.Scope, "s", "large", (1<<20)-3, 12)
		require.NoError(t, err)
		require.Equal(t, data[(1<<20)-3:(1<<20)+9], chunk)
		require.Equal(t, len(data), total)
		hash := sha256.Sum256(data)
		require.Equal(t, hex.EncodeToString(hash[:]), digest)
		entries, err := s.History(ctx, r.Scope, "s", 0, 50)
		require.NoError(t, err)
		require.Len(t, entries, 2)
		require.Empty(t, entries[0].Data)
		require.NotEmpty(t, entries[0].ContentRef)
		require.JSONEq(t, `"hello"`, string(entries[1].Data))
		_, _, _, err = s.Chunk(ctx, r.Scope, "s", "private", 0, 20)
		require.ErrorIs(t, err, mc.ErrNotFound)
		_, _, _, err = s.Chunk(ctx, "different", "s", "large", 0, 20)
		require.ErrorIs(t, err, mc.ErrNotFound)
		_, _, _, err = s.Chunk(ctx, r.Scope, "wrong-session", "large", 0, 20)
		require.ErrorIs(t, err, mc.ErrNotFound)
		revision := r.Revision
		err = s.CAS(ctx, r, &l, []mc.Blob{contextBlob("new", "s", "public", []byte(`"new"`)), contextBlob("large", "s", "public", []byte(`"conflict"`))}, nil)
		require.Error(t, err)
		require.Equal(t, revision, r.Revision)
		_, err = s.Blob(ctx, r.Scope, "new")
		require.ErrorIs(t, err, mc.ErrNotFound)
		stored, err := s.Read(ctx, r.Scope, r.Kind, r.ID)
		require.NoError(t, err)
		require.Equal(t, revision, stored.Revision)
		_, err = db.Exec(`UPDATE context_blob_parts SET data='corrupt' WHERE id='large' AND part=0`)
		require.NoError(t, err)
		_, err = s.Blob(ctx, r.Scope, "large")
		require.Error(t, err)
	})
	t.Run("physical_results_count_towards_user_quota", func(t *testing.T) {
		db := contextTestDB(t)
		s := NewContextSessionStore(db)
		p := mc.DefaultPolicy()
		p.MaxHistoryBytes = 1 << 20
		p.MaxUserHistoryBytes = 1500 << 10
		r := contextRecord("session", "s", 1, 2)
		_, err := s.Create(ctx, r, &p)
		require.NoError(t, err)
		l, err := s.Acquire(ctx, r.Scope, r.Kind, r.ID, "owner", time.Minute)
		require.NoError(t, err)
		a := contextRecord("attempt", "a", 1, 2)
		a.Data = json.RawMessage(`{"session_id":"s","settled":false}`)
		_, err = s.Create(ctx, a, nil)
		require.NoError(t, err)
		require.NoError(t, s.CAS(ctx, a, nil, []mc.Blob{contextBlob("physical", "s", "private", bytes.Repeat([]byte("x"), 800<<10))}, &p))
		require.NoError(t, s.CAS(ctx, r, &l, nil, &p))
		var data struct {
			Bytes int64 `json:"history_bytes"`
		}
		require.NoError(t, json.Unmarshal(r.Data, &data))
		require.EqualValues(t, 800<<10, data.Bytes)
		err = s.CAS(ctx, a, nil, []mc.Blob{contextBlob("too-large", "s", "private", bytes.Repeat([]byte("x"), 300<<10))}, &p)
		require.Error(t, err)
		_, err = s.Blob(ctx, r.Scope, "too-large")
		require.ErrorIs(t, err, mc.ErrNotFound)
		b := contextRecord("operation", "b", 1, 9)
		_, err = s.Create(ctx, b, nil)
		require.NoError(t, err)
		err = s.CAS(ctx, b, nil, []mc.Blob{contextBlob("other-key", "", "private", bytes.Repeat([]byte("y"), 800<<10))}, &p)
		require.Error(t, err, "quota spans API keys and includes private/stateless content")
	})
	t.Run("user_daily_maintenance_admission", func(t *testing.T) {
		db := contextTestDB(t)
		store := NewContextSessionStore(db)
		p := mc.DefaultPolicy()
		p.MaxUserMaintenanceCostUSD = .003
		for n, id := range []string{"first", "second"} {
			a := mc.Attempt{Owner: mc.Owner{Scope: "scope", UserID: 1, KeyID: int64(n + 2)}, ID: id, Purpose: "compaction", State: "prepared", EstimateUSD: .002, StartedAt: time.Now()}
			b, _ := json.Marshal(a)
			r := contextRecord("attempt", id, 1, int64(n+2))
			r.Data = b
			_, err := store.Create(ctx, r, &p)
			if n == 0 {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "maintenance budget spans all keys")
			}
		}
	})
	t.Run("concurrent_user_quota_and_recovery_retention", func(t *testing.T) {
		db := contextTestDB(t)
		s := NewContextSessionStore(db)
		p := mc.DefaultPolicy()
		p.MaxHistoryBytes = 1 << 20
		p.MaxUserHistoryBytes = 1500 << 10
		var wg sync.WaitGroup
		out := make(chan error, 2)
		for _, id := range []string{"a", "b"} {
			r := contextRecord("session", id, 1, 2)
			_, err := s.Create(ctx, r, &p)
			require.NoError(t, err)
			l, err := s.Acquire(ctx, r.Scope, r.Kind, r.ID, id, time.Minute)
			require.NoError(t, err)
			wg.Add(1)
			go func() {
				defer wg.Done()
				out <- s.CAS(ctx, r, &l, []mc.Blob{contextBlob("blob-"+r.ID, r.ID, "public", bytes.Repeat([]byte("x"), 800<<10))}, &p)
			}()
		}
		wg.Wait()
		close(out)
		success := 0
		for err := range out {
			if err == nil {
				success++
			}
		}
		require.Equal(t, 1, success)
		a := contextRecord("attempt", "unknown", 1, 2)
		a.Status = "unknown"
		a.Data = json.RawMessage(`{"session_id":"a","operation_id":"uncertain-op","settled":false}`)
		_, err := s.Create(ctx, a, nil)
		require.NoError(t, err)
		known := contextRecord("attempt", "known", 1, 2)
		known.Status = "terminal"
		known.Data = json.RawMessage(`{"session_id":"b","settled":false}`)
		_, err = s.Create(ctx, known, nil)
		require.NoError(t, err)
		pending, err := s.Pending(ctx, 1)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		require.Equal(t, "known", pending[0].ID)
		knownCount, unknownCount, err := s.Unsettled(ctx, a.Scope, "a")
		require.NoError(t, err)
		require.Zero(t, knownCount)
		require.Equal(t, 1, unknownCount)
		uncertain := contextRecord("operation", "uncertain-op", 1, 2)
		_, err = s.Create(ctx, uncertain, nil)
		require.NoError(t, err)
		_, err = db.Exec(`UPDATE context_records SET expires_at=NOW()-interval '1 day',lease_until=NULL`)
		require.NoError(t, err)
		_, err = s.Cleanup(ctx, 100)
		require.NoError(t, err)
		_, err = s.Read(ctx, a.Scope, "operation", "uncertain-op")
		require.NoError(t, err, "unknown operations must not be recreated and dispatched after TTL")
		_, err = s.Read(ctx, a.Scope, "attempt", a.ID)
		require.NoError(t, err)
		_, err = s.Read(ctx, a.Scope, "session", "a")
		require.NoError(t, err, "unknown attempts retain their session metadata")
		_, err = s.Read(ctx, a.Scope, "session", "missing")
		require.True(t, errors.Is(err, mc.ErrNotFound))
	})
}
