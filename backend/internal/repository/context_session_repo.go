package repository

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	mc "github.com/Wei-Shaw/sub2api/internal/pkg/managedcontext"
)

type contextSessionStore struct{ db *sql.DB }

func NewContextSessionStore(db *sql.DB) mc.Store { return &contextSessionStore{db: db} }

const contextColumns = `scope,kind,id,user_id,api_key_id,account_id,revision,status,data,expires_at,cancelled`

// An explicit reservation owner survives manual application clearing JobID.
// The job fallback also understands journals created before that field existed.
const contextReservationOperation = `CASE WHEN c.kind='operation' THEN c.id ELSE
 COALESCE(NULLIF(c.data->>'reservation_operation_id',''),NULLIF(c.data->>'operation_id',''),
 (SELECT j.data->>'operation_id' FROM context_records j WHERE j.scope=c.scope AND j.kind='job'
 AND j.id=COALESCE(NULLIF(c.data->>'job_id',''),c.data->>'applied_job_id')),'') END`

// Background allocations are immutable even after their unused capacity retires,
// so their share is never returned to a still-running foreground admission.
const contextReservationAllocated = `CASE WHEN c.kind='job' THEN 0 ELSE
 COALESCE((SELECT sum(COALESCE((j.data->>'allocated_bytes')::bigint,0))
 FROM context_records j WHERE j.scope=c.scope AND j.kind='job'
 AND j.data->>'operation_id'=` + contextReservationOperation + `),0) END`

const contextReservationConsumed = `COALESCE((SELECT sum(h.size_bytes)
 FROM context_records a JOIN context_history h ON h.scope=a.scope AND h.id=a.data->>'response_ref'
 WHERE a.scope=c.scope AND a.kind='attempt' AND a.data->>'operation_id'=` + contextReservationOperation + `
 AND (c.kind='job' AND a.data->>'reservation_id'=c.id
 OR c.kind<>'job' AND NULLIF(a.data->>'reservation_id','') IS NULL)),0)`

const contextReservationPending = `EXISTS(SELECT 1 FROM context_records a
 WHERE a.scope=c.scope AND a.kind='attempt'
 AND a.data->>'operation_id'=` + contextReservationOperation + `
 AND (c.kind='job' AND a.data->>'reservation_id'=c.id
 OR c.kind<>'job' AND NULLIF(a.data->>'reservation_id','') IS NULL)
 AND a.status IN ('prepared','dispatching','unknown')
 AND (a.data->>'deadline')::timestamptz+interval '120 seconds'>NOW())`

func scanContext(row interface{ Scan(...any) error }) (*mc.Record, error) {
	r := new(mc.Record)
	err := row.Scan(&r.Scope, &r.Kind, &r.ID, &r.UserID, &r.KeyID, &r.AccountID, &r.Revision, &r.Status, &r.Data, &r.ExpiresAt, &r.Cancelled)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, mc.ErrNotFound
	}
	return r, err
}
func (r *contextSessionStore) Read(ctx context.Context, scope, kind, id string) (*mc.Record, error) {
	return scanContext(r.db.QueryRowContext(ctx, `SELECT `+contextColumns+` FROM context_records WHERE scope=$1 AND kind=$2 AND id=$3`, scope, kind, id))
}
func contextOwnerLock(ctx context.Context, tx *sql.Tx, userID int64) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(-7300000000000000000)+userID)
	return err
}
func (r *contextSessionStore) Create(ctx context.Context, v *mc.Record, p *mc.Policy) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if p != nil {
		if err = contextOwnerLock(ctx, tx, v.UserID); err != nil {
			return false, err
		}
	}
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM context_records WHERE scope=$1 AND kind=$2 AND id=$3)`, v.Scope, v.Kind, v.ID).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	} // Idempotent reads remain available at quota.
	if v.Kind == "operation" && p != nil {
		if err = contextOwnerLock(ctx, tx, v.UserID); err != nil {
			return false, err
		}
		var total, keyCount int
		if err = tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE api_key_id=$2) FROM context_records WHERE user_id=$1 AND kind='operation' AND NOT(scope=$3 AND id=$4)`, v.UserID, v.KeyID, v.Scope, v.ID).Scan(&total, &keyCount); err != nil {
			return false, err
		}
		if total >= min(p.MaxRequests*p.MaxUserSessions, 65536) || keyCount >= min(p.MaxRequests*p.MaxSessions, 16384) {
			return false, mc.E("operation_limit", "retained context request record limit reached", 413)
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE api_key_id=$2) FROM context_records WHERE user_id=$1 AND kind='attempt' AND COALESCE((data->>'settled')::boolean,FALSE)=FALSE`, v.UserID, v.KeyID).Scan(&total, &keyCount); err != nil {
			return false, err
		}
		if total >= p.MaxUserSessions*p.MaxTurnRequests || keyCount >= p.MaxSessions*p.MaxTurnRequests {
			return false, mc.E("settlement_limit", "unresolved attempt limit reached", 409)
		}
	}
	if v.Kind == "attempt" && p != nil {
		var a mc.Attempt
		if err = json.Unmarshal(v.Data, &a); err != nil {
			return false, err
		}
		if strings.HasPrefix(a.Purpose, "compaction") {
			if err = contextOwnerLock(ctx, tx, v.UserID); err != nil {
				return false, err
			}
			var spent float64
			if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(CASE WHEN COALESCE((data->>'settled')::boolean,FALSE) THEN COALESCE((data->>'cost_usd')::double precision,0) ELSE COALESCE((data->>'estimate_usd')::double precision,0) END),0) FROM context_records WHERE user_id=$1 AND kind='attempt' AND data->>'purpose' LIKE 'compaction%' AND (data->>'started_at')::timestamptz>NOW()-interval '24 hours' AND NOT(scope=$2 AND id=$3)`, v.UserID, v.Scope, v.ID).Scan(&spent); err != nil {
				return false, err
			}
			if spent+a.EstimateUSD > p.MaxUserMaintenanceCostUSD {
				return false, mc.E("budget_exhausted", "user daily maintenance cost budget reached", 402)
			}
		}
	}
	if v.Kind == "session" && p != nil {
		if err = contextOwnerLock(ctx, tx, v.UserID); err != nil {
			return false, err
		}
		var total, keyCount int
		if err = tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE api_key_id=$2) FROM context_records WHERE user_id=$1 AND kind='session' AND expires_at>NOW() AND NOT cancelled AND status<>'failed' AND NOT (scope=$3 AND id=$4)`, v.UserID, v.KeyID, v.Scope, v.ID).Scan(&total, &keyCount); err != nil {
			return false, err
		}
		if total >= p.MaxUserSessions || keyCount >= p.MaxSessions {
			return false, mc.E("session_limit", "active managed session limit reached", 413)
		}
		if err = tx.QueryRowContext(ctx, `SELECT count(*),count(*) FILTER(WHERE api_key_id=$2) FROM context_records WHERE user_id=$1 AND kind='attempt' AND status<>'prepared' AND COALESCE((data->>'settled')::boolean,FALSE)=FALSE`, v.UserID, v.KeyID).Scan(&total, &keyCount); err != nil {
			return false, err
		}
		if total >= p.MaxUserSessions*p.MaxTurnRequests || keyCount >= p.MaxSessions*p.MaxTurnRequests {
			return false, mc.E("settlement_limit", "unresolved attempt limit reached", 409)
		}
	}
	var rev int64
	err = tx.QueryRowContext(ctx, `INSERT INTO context_records(scope,kind,id,user_id,api_key_id,account_id,status,data,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(scope,kind,id) DO NOTHING RETURNING revision`, v.Scope, v.Kind, v.ID, v.UserID, v.KeyID, v.AccountID, v.Status, []byte(v.Data), v.ExpiresAt).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	v.Revision = rev
	return true, nil
}
func (r *contextSessionStore) CAS(ctx context.Context, v *mc.Record, lease *mc.Lease, blobs []mc.Blob, p *mc.Policy) error {
	if lease != nil && (lease.Scope != v.Scope || lease.Kind != v.Kind || lease.ID != v.ID) {
		return mc.E("lease_lost", "execution lease belongs to another resource", 409)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if v.Kind == "session" && lease == nil {
		return mc.E("lease_lost", "session commit requires execution ownership", 409)
	}
	if p != nil {
		if err = contextOwnerLock(ctx, tx, v.UserID); err != nil {
			return err
		}
		if err = checkContextQuota(ctx, tx, v, blobs, p); err != nil {
			return err
		}
	}
	query := `UPDATE context_records SET data=$1,status=$2,expires_at=$3,revision=revision+1,updated_at=NOW(),account_id=$8 WHERE scope=$4 AND kind=$5 AND id=$6 AND revision=$7`
	args := []any{[]byte(v.Data), v.Status, v.ExpiresAt, v.Scope, v.Kind, v.ID, v.Revision, v.AccountID}
	if lease != nil {
		query += ` AND lease_owner=$9 AND lease_token=$10 AND lease_until>NOW()`
		args = append(args, lease.Owner, lease.Token)
	}
	if v.Kind == "session" {
		query += ` AND NOT cancelled`
	}
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return mc.E("revision_conflict", "context changed, was cancelled, or execution ownership expired", 409)
	}
	for _, b := range blobs {
		if err = writeContextBlob(ctx, tx, v.Owner, b); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	v.Revision++
	return nil
}

// Quotas include every retained blob, including physical responses and private
// summaries. Reservations are counted before admission; terminal writers can
// consume an existing reservation without needing the session's execution lease.
func checkContextQuota(ctx context.Context, tx *sql.Tx, v *mc.Record, blobs []mc.Blob, p *mc.Policy) error {
	var total, sessionBytes, reserved int64
	sessionID := ""
	var metadata map[string]json.RawMessage
	if err := jsonUnmarshalContext(v.Data, &metadata); err != nil {
		return err
	}
	if v.Kind == "session" {
		sessionID = v.ID
	} else {
		_ = json.Unmarshal(metadata["session_id"], &sessionID)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(size_bytes),0),COALESCE(sum(size_bytes) FILTER(WHERE scope=$2 AND session_id=$3 AND session_id<>''),0) FROM context_history WHERE user_id=$1`, v.UserID, v.Scope, sessionID).Scan(&total, &sessionBytes); err != nil {
		return err
	}
	var incoming int64
	seen := map[string]bool{}
	for _, b := range blobs {
		if seen[b.ID] {
			continue
		}
		seen[b.ID] = true
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM context_history WHERE scope=$1 AND id=$2)`, v.Scope, b.ID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			incoming += int64(len(b.Data))
		}
	}
	// Keep every unrelated promise of storage. The physical results already
	// written for an admission's current operation consume its reservation;
	// deriving that credit avoids changing the session revision from a late
	// attempt writer which does not own its execution lease.
	// Cancelled/expired admissions retain capacity while a live lease could
	// still journal a call, or an attempt could still persist a late result:
	// its execution deadline plus the 90s provider drain and 30s persistence.
	rows, err := tx.QueryContext(ctx, `SELECT c.scope,c.kind,c.id,c.data,`+contextReservationOperation+`,
 `+contextReservationConsumed+`,`+contextReservationAllocated+`
 FROM context_records c WHERE c.user_id=$1 AND c.kind IN ('session','operation','job')
 AND (c.data->>'reserved_bytes')::bigint>0
 AND (c.kind<>'job' AND (NOT c.cancelled AND c.expires_at>NOW() AND c.status NOT IN ('failed','completed')
 AND NOT EXISTS(SELECT 1 FROM context_records finished_job
 WHERE finished_job.scope=c.scope AND finished_job.kind='job'
 AND (c.kind='operation' OR NULLIF(c.data->>'operation_id','') IS NULL)
 AND finished_job.id=COALESCE(NULLIF(c.data->>'job_id',''),c.data->>'applied_job_id')
 AND finished_job.data->>'operation_id'=`+contextReservationOperation+`
 AND finished_job.status IN ('failed','stale','applied','compacted','no_op'))
 OR c.lease_until>NOW()) OR `+contextReservationPending+`)
 AND NOT(c.scope=$2 AND c.kind=$3 AND c.id=$4)`, v.UserID, v.Scope, v.Kind, v.ID)
	if err != nil {
		return err
	}
	type reservation struct {
		scope, kind, id string
		operation       string
		data            []byte
		consumed        int64
		allocated       int64
	}
	var admissions []reservation
	for rows.Next() {
		var admission reservation
		if err = rows.Scan(&admission.scope, &admission.kind, &admission.id, &admission.data, &admission.operation, &admission.consumed, &admission.allocated); err != nil {
			_ = rows.Close()
			return err
		}
		admissions = append(admissions, admission)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if v.Kind == "session" || v.Kind == "operation" || v.Kind == "job" {
		var consumed, allocated int64
		opID := ""
		if err = tx.QueryRowContext(ctx, `SELECT `+contextReservationOperation+`,`+contextReservationConsumed+`,`+contextReservationAllocated+` FROM
 (SELECT $1::text AS scope,$2::text AS kind,$3::text AS id,$4::jsonb AS data) c`, v.Scope, v.Kind, v.ID, []byte(v.Data)).Scan(&opID, &consumed, &allocated); err != nil {
			return err
		}
		admissions = append(admissions, reservation{scope: v.Scope, kind: v.Kind, id: v.ID, operation: opID, data: v.Data, consumed: consumed, allocated: allocated})
	}
	var attemptOperation, attemptReservation string
	if v.Kind == "attempt" {
		_ = json.Unmarshal(metadata["operation_id"], &attemptOperation)
		_ = json.Unmarshal(metadata["reservation_id"], &attemptReservation)
	}
	var sessionReserved int64
	for _, admission := range admissions {
		var data struct {
			Reserved  int64  `json:"reserved_bytes"`
			SessionID string `json:"session_id"`
		}
		if err = jsonUnmarshalContext(admission.data, &data); err != nil {
			return err
		}
		if data.Reserved < 0 || admission.allocated < 0 {
			return mc.E("policy_invalid", "invalid storage reservation", 503)
		}
		if admission.kind == "session" {
			data.SessionID = admission.id
		}
		if attemptOperation != "" && admission.scope == v.Scope && admission.operation == attemptOperation &&
			(admission.kind == "job" && admission.id == attemptReservation || admission.kind != "job" && attemptReservation == "") {
			admission.consumed += incoming
		}
		remaining := max(data.Reserved-admission.allocated-admission.consumed, 0)
		reserved += remaining
		if sessionID != "" && admission.scope == v.Scope && data.SessionID == sessionID {
			sessionReserved += remaining
		}
	}
	if total+incoming+reserved > p.MaxUserHistoryBytes || sessionID != "" && sessionBytes+incoming+sessionReserved > p.MaxHistoryBytes {
		return mc.E("history_limit", "managed history storage budget reached", 413)
	}
	if v.Kind == "session" {
		metadata["history_bytes"], _ = json.Marshal(sessionBytes + incoming)
		var err error
		v.Data, err = json.Marshal(metadata)
		return err
	}
	return nil
}

func writeContextBlob(ctx context.Context, tx *sql.Tx, owner mc.Owner, b mc.Blob) error {
	scope := owner.Scope
	if len(b.Data) > 64<<20 {
		return mc.E("history_limit", "one historical item exceeds the storage limit", 413)
	}
	h := sha256.Sum256(b.Data)
	digest := hex.EncodeToString(h[:])
	if b.Digest != "" && b.Digest != digest {
		return mc.E("source_invalid", "history digest mismatch", 503)
	}
	const partSize = 1 << 20
	parts := max(1, (len(b.Data)+partSize-1)/partSize)
	res, err := tx.ExecContext(ctx, `INSERT INTO context_history(scope,id,session_id,visibility,kind,digest,size_bytes,parts,expires_at,user_id,api_key_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) ON CONFLICT(scope,id) DO NOTHING`, scope, b.ID, b.SessionID, b.Visibility, b.Kind, digest, len(b.Data), parts, b.ExpiresAt, owner.UserID, owner.KeyID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		var existing, session, visibility, kind string
		var size int
		if err = tx.QueryRowContext(ctx, `SELECT digest,session_id,visibility,kind,size_bytes FROM context_history WHERE scope=$1 AND id=$2`, scope, b.ID).Scan(&existing, &session, &visibility, &kind, &size); err != nil {
			return err
		}
		if existing != digest || session != b.SessionID || visibility != b.Visibility || kind != b.Kind || size != len(b.Data) {
			return mc.E("source_invalid", "immutable history ID conflicts with existing data", 409)
		}
		return nil
	}
	for i := 0; i < parts; i++ {
		lo := i * partSize
		hi := min(lo+partSize, len(b.Data))
		if _, err = tx.ExecContext(ctx, `INSERT INTO context_blob_parts(scope,id,part,data) VALUES($1,$2,$3,$4)`, scope, b.ID, i, b.Data[lo:hi]); err != nil {
			return err
		}
	}
	return nil
}
func (r *contextSessionStore) Acquire(ctx context.Context, scope, kind, id, owner string, ttl time.Duration) (mc.Lease, error) {
	l := mc.Lease{Scope: scope, Kind: kind, ID: id, Owner: owner}
	err := r.db.QueryRowContext(ctx, `UPDATE context_records SET lease_owner=$4,lease_token=lease_token+1,lease_until=NOW()+($5 * interval '1 millisecond') WHERE scope=$1 AND kind=$2 AND id=$3 AND (lease_until IS NULL OR lease_until<=NOW()) RETURNING lease_token`, scope, kind, id, owner, ttl.Milliseconds()).Scan(&l.Token)
	if errors.Is(err, sql.ErrNoRows) {
		return l, mc.E("busy", "another request owns this context operation", 409)
	}
	return l, err
}
func (r *contextSessionStore) Renew(ctx context.Context, l mc.Lease, ttl time.Duration) error {
	result, err := r.db.ExecContext(ctx, `UPDATE context_records SET lease_until=NOW()+($6 * interval '1 millisecond') WHERE scope=$1 AND kind=$2 AND id=$3 AND lease_owner=$4 AND lease_token=$5 AND lease_until>NOW()`, l.Scope, l.Kind, l.ID, l.Owner, l.Token, ttl.Milliseconds())
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return mc.E("lease_lost", "execution ownership expired", 409)
	}
	return nil
}
func (r *contextSessionStore) Release(ctx context.Context, l mc.Lease) error {
	_, err := r.db.ExecContext(ctx, `UPDATE context_records SET lease_until=NULL,lease_owner='' WHERE scope=$1 AND kind=$2 AND id=$3 AND lease_owner=$4 AND lease_token=$5`, l.Scope, l.Kind, l.ID, l.Owner, l.Token)
	return err
}
func (r *contextSessionStore) Cancel(ctx context.Context, scope, id string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE context_records SET cancelled=TRUE,status='cancelled',revision=revision+1,updated_at=NOW() WHERE scope=$1 AND kind='session' AND id=$2`, scope, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return mc.ErrNotFound
	}
	return nil
}
func (r *contextSessionStore) Blob(ctx context.Context, scope, id string) ([]byte, error) {
	var digest string
	var size, parts int
	if err := r.db.QueryRowContext(ctx, `SELECT digest,size_bytes,parts FROM context_history WHERE scope=$1 AND id=$2`, scope, id).Scan(&digest, &size, &parts); errors.Is(err, sql.ErrNoRows) {
		return nil, mc.ErrNotFound
	} else if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT part,data FROM context_blob_parts WHERE scope=$1 AND id=$2 ORDER BY part`, scope, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	data := make([]byte, 0, size)
	seen := 0
	for rows.Next() {
		var i int
		var b []byte
		if err = rows.Scan(&i, &b); err != nil {
			return nil, err
		}
		if i != seen || len(data)+len(b) > size {
			return nil, mc.E("source_invalid", "history parts failed validation", 503)
		}
		data = append(data, b...)
		seen++
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	h := sha256.Sum256(data)
	if seen != parts || len(data) != size || hex.EncodeToString(h[:]) != digest {
		return nil, mc.E("source_invalid", "history content failed validation", 503)
	}
	return data, nil
}
func (r *contextSessionStore) History(ctx context.Context, scope, session string, cursor int64, limit int) ([]mc.HistoryEntry, error) {
	limit = min(max(limit, 1), 50)
	rows, err := r.db.QueryContext(ctx, `SELECT seq,id,size_bytes,digest,kind FROM context_history WHERE scope=$1 AND session_id=$2 AND visibility='public' AND seq>$3 ORDER BY seq LIMIT $4`, scope, session, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []mc.HistoryEntry{}
	for rows.Next() {
		var v mc.HistoryEntry
		if err = rows.Scan(&v.Sequence, &v.ID, &v.Bytes, &v.Digest, &v.Kind); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	// Close the cursor before fetching payloads, including with a one-connection pool.
	if err = rows.Close(); err != nil {
		return nil, err
	}
	used := 0
	for i := range out {
		v := &out[i]
		v.ContentRef = v.ID
		if v.Bytes <= 256<<10 && used+v.Bytes <= 1<<20 {
			v.Data, err = r.Blob(ctx, scope, v.ID)
			if err != nil {
				return nil, err
			}
			v.ContentRef = ""
			used += v.Bytes
		}
	}
	return out, nil
}
func (r *contextSessionStore) Chunk(ctx context.Context, scope, session, id string, offset, limit int) ([]byte, int, string, error) {
	var size int
	var digest string
	err := r.db.QueryRowContext(ctx, `SELECT size_bytes,digest FROM context_history WHERE scope=$1 AND session_id=$2 AND id=$3 AND visibility='public'`, scope, session, id).Scan(&size, &digest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, "", mc.ErrNotFound
	}
	if err != nil {
		return nil, 0, "", err
	}
	if offset < 0 || offset > size {
		return nil, 0, "", mc.E("input_invalid", "history offset is out of range", 400)
	}
	limit = min(max(limit, 1), 256<<10)
	end := min(offset+limit, size)
	data := []byte{}
	for start := offset; start < end; {
		part := start / (1 << 20)
		within := start % (1 << 20)
		take := min(end-start, (1<<20)-within)
		var b []byte
		if err = r.db.QueryRowContext(ctx, `SELECT substring(data FROM $4 FOR $5) FROM context_blob_parts WHERE scope=$1 AND id=$2 AND part=$3`, scope, id, part, within+1, take).Scan(&b); err != nil {
			return nil, 0, "", err
		}
		if len(b) != take {
			return nil, 0, "", mc.E("source_invalid", "historical chunk is incomplete", 503)
		}
		data = append(data, b...)
		start += take
	}
	return data, size, digest, nil
}
func (r *contextSessionStore) Pending(ctx context.Context, limit int) ([]*mc.Record, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+contextColumns+` FROM context_records WHERE kind='attempt' AND (status IN ('terminal','dispatching','unknown') OR status='prepared' AND (data->>'deadline')::timestamptz<NOW()) AND COALESCE((data->>'settled')::boolean,FALSE)=FALSE ORDER BY CASE WHEN status='terminal' THEN 0 WHEN status='prepared' THEN 1 WHEN status='dispatching' THEN 2 ELSE 3 END, updated_at LIMIT $1`, min(max(limit, 1), 100))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []*mc.Record{}
	for rows.Next() {
		v, err := scanContext(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (r *contextSessionStore) Unsettled(ctx context.Context, scope, session string) (known, unknown int, err error) {
	err = r.db.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE status='terminal'),count(*) FILTER(WHERE status IN ('dispatching','unknown')) FROM context_records WHERE scope=$1 AND kind='attempt' AND data->>'session_id'=$2 AND COALESCE((data->>'settled')::boolean,FALSE)=FALSE`, scope, session).Scan(&known, &unknown)
	return
}
func (r *contextSessionStore) UsageStats(ctx context.Context, scope, session string) (mc.UsageStats, error) {
	var out mc.UsageStats
	rows, err := r.db.QueryContext(ctx, `SELECT data FROM context_records WHERE scope=$1 AND kind='attempt' AND data->>'session_id'=$2`, scope, session)
	if err != nil {
		return out, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return out, err
		}
		var a mc.Attempt
		if err = json.Unmarshal(raw, &a); err != nil {
			return out, err
		}
		if !a.Settled {
			if a.State == "terminal" {
				out.Known++
			}
			if a.State == "dispatching" || a.State == "unknown" {
				out.Unknown++
			}
		}
		if strings.HasPrefix(a.Purpose, "compaction") {
			out.Maintenance = mergeContextUsage(out.Maintenance, a.Usage)
			out.MaintenanceCostUSD += a.CostUSD
		} else {
			out.Inference = mergeContextUsage(out.Inference, a.Usage)
		}
	}
	return out, rows.Err()
}
func mergeContextUsage(a, b *apicompat.ResponsesUsage) *apicompat.ResponsesUsage {
	if b == nil {
		return a
	}
	if a == nil {
		a = &apicompat.ResponsesUsage{}
	}
	a.InputTokens += b.InputTokens
	a.OutputTokens += b.OutputTokens
	a.TotalTokens += b.TotalTokens
	a.CacheCreationInputTokens += b.CacheCreationInputTokens
	if b.InputTokensDetails != nil {
		if a.InputTokensDetails == nil {
			a.InputTokensDetails = &apicompat.ResponsesInputTokensDetails{}
		}
		a.InputTokensDetails.CachedTokens += b.InputTokensDetails.CachedTokens
	}
	return a
}
func (r *contextSessionStore) Cleanup(ctx context.Context, limit int) (int64, error) {
	limit = min(max(limit, 1), 500)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM context_history WHERE seq IN (SELECT h.seq FROM context_history h WHERE h.expires_at<NOW()
 AND NOT EXISTS (SELECT 1 FROM context_records a WHERE a.scope=h.scope AND a.kind='attempt'
 AND ((a.data->>'session_id'<>'' AND a.data->>'session_id'=h.session_id) OR a.data->>'response_ref'=h.id OR EXISTS(SELECT 1 FROM context_records o WHERE o.scope=a.scope AND o.kind='operation' AND o.id=a.data->>'operation_id' AND o.data->>'response_ref'=h.id))
 AND (COALESCE((a.data->>'settled')::boolean,FALSE)=FALSE OR a.updated_at>NOW()-interval '24 hours'))
 ORDER BY h.expires_at LIMIT $1)`, limit)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM context_records WHERE (scope,kind,id) IN (SELECT scope,kind,id FROM context_records WHERE expires_at<NOW() AND (lease_until IS NULL OR lease_until<NOW()) AND (kind<>'attempt' OR status='prepared' OR (COALESCE((data->>'settled')::boolean,FALSE)=TRUE AND updated_at<NOW()-interval '24 hours')) AND NOT EXISTS (SELECT 1 FROM context_records a WHERE a.scope=context_records.scope AND a.kind='attempt' AND ((context_records.kind='session' AND a.data->>'session_id'=context_records.id) OR (context_records.kind='operation' AND a.data->>'operation_id'=context_records.id)) AND (COALESCE((a.data->>'settled')::boolean,FALSE)=FALSE OR a.updated_at>NOW()-interval '24 hours')) ORDER BY expires_at LIMIT $1)`, limit)
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}

// Kept local to make malformed persisted quota metadata fail closed.
func jsonUnmarshalContext(b []byte, v any) error {
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("invalid context quota metadata: %w", err)
	}
	return nil
}
