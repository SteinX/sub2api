package managedcontext

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	cc "github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
	"github.com/stretchr/testify/require"
)

type memStore struct {
	mu       sync.Mutex
	records  map[string]Record
	blobs    map[string]Blob
	leases   map[string]Lease
	until    map[string]time.Time
	sequence map[string]int64
	seq      int64
	fail     func(*Record) bool
}

func newMem() *memStore {
	return &memStore{records: map[string]Record{}, blobs: map[string]Blob{}, leases: map[string]Lease{}, until: map[string]time.Time{}, sequence: map[string]int64{}}
}
func rk(scope, kind, id string) string { return scope + ":" + kind + ":" + id }
func (m *memStore) Read(_ context.Context, scope, kind, id string) (*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.records[rk(scope, kind, id)]
	if !ok {
		return nil, ErrNotFound
	}
	v.Data = append([]byte(nil), v.Data...)
	return &v, nil
}
func (m *memStore) Create(_ context.Context, r *Record, _ *Policy) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := rk(r.Scope, r.Kind, r.ID)
	if _, ok := m.records[key]; ok {
		return false, nil
	}
	r.Revision = 1
	v := *r
	v.Data = append([]byte(nil), r.Data...)
	m.records[key] = v
	return true, nil
}
func (m *memStore) CAS(_ context.Context, r *Record, l *Lease, blobs []Blob, _ *Policy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := rk(r.Scope, r.Kind, r.ID)
	old, ok := m.records[key]
	if !ok || old.Revision != r.Revision || r.Kind == "session" && old.Cancelled {
		return E("revision_conflict", "changed", 409)
	}
	if l != nil {
		lease := m.leases[key]
		if lease.Owner != l.Owner || lease.Token != l.Token || !time.Now().Before(m.until[key]) {
			return E("lease_lost", "lost", 409)
		}
	}
	if m.fail != nil && m.fail(r) {
		return errors.New("injected persistence failure")
	}
	for _, b := range blobs {
		bk := rk(r.Scope, "blob", b.ID)
		if old, ok := m.blobs[bk]; ok && string(old.Data) != string(b.Data) {
			return errors.New("blob conflict")
		}
	}
	r.Revision++
	v := *r
	v.Data = append([]byte(nil), r.Data...)
	m.records[key] = v
	for _, b := range blobs {
		bk := rk(r.Scope, "blob", b.ID)
		if _, ok := m.blobs[bk]; !ok {
			m.seq++
			m.sequence[bk] = m.seq
		}
		b.Data = append([]byte(nil), b.Data...)
		m.blobs[bk] = b
	}
	return nil
}
func (m *memStore) Acquire(_ context.Context, scope, kind, id, owner string, ttl time.Duration) (Lease, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := rk(scope, kind, id)
	if _, ok := m.records[key]; !ok {
		return Lease{}, ErrNotFound
	}
	old := m.leases[key]
	if time.Now().Before(m.until[key]) {
		return Lease{}, E("busy", "busy", 409)
	}
	l := Lease{Scope: scope, Kind: kind, ID: id, Owner: owner, Token: old.Token + 1}
	m.leases[key] = l
	m.until[key] = time.Now().Add(ttl)
	return l, nil
}
func (m *memStore) Renew(_ context.Context, l Lease, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := rk(l.Scope, l.Kind, l.ID)
	if m.leases[key] != l || !time.Now().Before(m.until[key]) {
		return E("lease_lost", "lost", 409)
	}
	m.until[key] = time.Now().Add(ttl)
	return nil
}
func (m *memStore) Release(_ context.Context, l Lease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := rk(l.Scope, l.Kind, l.ID)
	if m.leases[key] == l {
		m.until[key] = time.Time{}
	}
	return nil
}
func (m *memStore) Cancel(_ context.Context, scope, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := rk(scope, "session", id)
	r, ok := m.records[key]
	if !ok {
		return ErrNotFound
	}
	r.Cancelled = true
	r.Revision++
	m.records[key] = r
	return nil
}
func (m *memStore) Blob(_ context.Context, scope, id string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[rk(scope, "blob", id)]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), b.Data...), nil
}
func (m *memStore) History(_ context.Context, scope, session string, cursor int64, limit int) ([]HistoryEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []HistoryEntry
	for k, b := range m.blobs {
		seq := m.sequence[k]
		if strings.HasPrefix(k, scope+":blob:") && b.SessionID == session && b.Visibility == "public" && seq > cursor {
			out = append(out, HistoryEntry{Sequence: seq, ID: b.ID, Bytes: len(b.Data), Data: append([]byte(nil), b.Data...)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (m *memStore) Chunk(ctx context.Context, scope, session, id string, offset, limit int) ([]byte, int, string, error) {
	b, err := m.Blob(ctx, scope, id)
	if err != nil {
		return nil, 0, "", err
	}
	if offset < 0 || offset > len(b) {
		return nil, 0, "", errors.New("offset")
	}
	return b[offset:min(offset+limit, len(b))], len(b), digestBytes(b), nil
}
func (m *memStore) Pending(_ context.Context, limit int) ([]*Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Record
	for _, r := range m.records {
		if r.Kind == "attempt" {
			a, _ := decode[Attempt](&r)
			if !a.Settled {
				copy := r
				out = append(out, &copy)
			}
		}
	}
	return out, nil
}
func (m *memStore) Unsettled(_ context.Context, scope, session string) (known, unknown int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.records {
		if r.Scope == scope && r.Kind == "attempt" {
			a, _ := decode[Attempt](&r)
			if a.SessionID == session && !a.Settled {
				if a.State == "terminal" {
					known++
				}
				if a.State == "unknown" || a.State == "dispatching" {
					unknown++
				}
			}
		}
	}
	return
}
func (m *memStore) UsageStats(ctx context.Context, scope, session string) (UsageStats, error) {
	k, u, err := m.Unsettled(ctx, scope, session)
	return UsageStats{Known: k, Unknown: u}, err
}
func (m *memStore) Cleanup(context.Context, int) (int64, error) { return 0, nil }

type fakeExecutor struct {
	mu              sync.Mutex
	calls           []Call
	settled         map[string]bool
	charges         int
	safe            int
	tool            bool
	unknown         bool
	failSettle      bool
	outputCap       int
	summaryText     string
	beforeReturn    func(Call)
	allowAux        bool
	late            bool
	started, finish chan struct{}
	once            sync.Once
}

func (x *fakeExecutor) OutputLimit() int { return x.outputCap }
func (x *fakeExecutor) Measure(r *apicompat.ResponsesRequest) (cc.Budget, error) {
	b, _ := json.Marshal(r)
	safe := x.safe
	if safe == 0 {
		safe = 2 << 20
	}
	return cc.Budget{InputEstimate: len(b), SafeInput: safe, Method: cc.ConservativeUTF8V1}, nil
}
func (x *fakeExecutor) Estimate(context.Context, Call) (float64, error) { return .001, nil }
func (x *fakeExecutor) Execute(ctx context.Context, c Call) (Outcome, error) {
	x.mu.Lock()
	x.calls = append(x.calls, c)
	tool, unknown := x.tool, x.unknown
	x.mu.Unlock()
	if unknown {
		return Outcome{}, errors.New("connection lost")
	}
	text := `{"goal":"Preserve constraints","current_state":"Historical tools summarized","decisions":[],"changes":[],"evidence":["Observed tool results"],"checks":[],"open_questions":[],"next_steps":[],"files":[]}`
	output := []apicompat.ResponsesOutput{{Type: "message", Role: "assistant", Content: []apicompat.ResponsesContentPart{{Type: "output_text", Text: "done"}}}}
	if strings.HasPrefix(c.Purpose, "compaction") {
		if x.started != nil {
			x.once.Do(func() { close(x.started) })
			if x.late {
				<-x.finish
			} else {
				select {
				case <-x.finish:
				case <-ctx.Done():
					return Outcome{}, ctx.Err()
				}
			}
		}
		output[0].Content[0].Text = text
		if x.summaryText != "" {
			output[0].Content[0].Text = x.summaryText
		}
	} else if tool {
		output = []apicompat.ResponsesOutput{{Type: "function_call", CallID: "call_" + c.ID, Name: "read", Arguments: `{}`}}
	}
	if x.beforeReturn != nil {
		x.beforeReturn(c)
	}
	return Outcome{Response: &apicompat.ResponsesResponse{ID: "physical_" + c.ID, Status: "completed", Model: c.Model, Output: output, Usage: &apicompat.ResponsesUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}}, BillingModel: c.Model}, nil
}
func (x *fakeExecutor) Settle(_ context.Context, a Attempt) (float64, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.failSettle {
		x.failSettle = false
		return 0, errors.New("billing unavailable")
	}
	if x.settled == nil {
		x.settled = map[string]bool{}
	}
	if !x.settled[a.ID] {
		x.settled[a.ID] = true
		x.charges++
	}
	return .001, nil
}
func (x *fakeExecutor) Auxiliary(context.Context) (Executor, func(), bool) {
	if x.allowAux {
		return x, func() {}, true
	}
	return nil, nil, false
}
func (x *fakeExecutor) callCount() int { x.mu.Lock(); defer x.mu.Unlock(); return len(x.calls) }
func owner() Owner                     { return Owner{Scope: "scope", UserID: 1, KeyID: 2, AccountID: 7} }
func parsed(t *testing.T, input any, prev string) Parsed {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"model": "concrete", "input": input, "previous_response_id": prev, "tools": []any{map[string]any{"type": "function", "name": "read", "parameters": map[string]any{"type": "object"}}}})
	p, err := Parse(b, false)
	require.NoError(t, err)
	return p
}
func run(t *testing.T, e *Engine, x Executor, key, session string, p Parsed) (*Result, error) {
	t.Helper()
	op, _, err := e.Begin(context.Background(), owner(), "responses:"+session, key, cc.Digest(p.Request), "concrete", session, DefaultPolicy())
	if err != nil {
		return nil, err
	}
	var out *Result
	err = e.WithOperation(context.Background(), op, func(ctx context.Context, op *Operation, r *Record, l *Lease) error {
		response, err := e.RecoverPublished(ctx, op, r, l)
		if response != nil || err != nil {
			out = &Result{Response: response, SessionID: op.SessionID, Replayed: true}
			return err
		}
		out, err = e.Advance(ctx, op, r, l, p, x)
		return err
	})
	return out, err
}
func TestManagedToolContinuationIdempotencyAndScope(t *testing.T) {
	e := New(newMem())
	x := &fakeExecutor{tool: true}
	p := parsed(t, "Keep exact public API", "")
	first, err := run(t, e, x, "first", "", p)
	require.NoError(t, err)
	require.NotEmpty(t, first.SessionID)
	replay, err := run(t, e, x, "first", "", p)
	require.NoError(t, err)
	require.Equal(t, first.Response.ID, replay.Response.ID)
	require.Equal(t, 1, x.callCount())
	require.Equal(t, 1, x.charges)
	s, _, err := e.LoadSession(context.Background(), owner().Scope, first.SessionID)
	require.NoError(t, err)
	require.NotEmpty(t, s.PendingCall)
	_, err = run(t, e, x, "bad", s.ID, parsed(t, "Another user input", s.Head))
	require.Error(t, err)
	require.Equal(t, 1, x.callCount())
	p = parsed(t, []cc.Item{{Type: "function_call_output", CallID: s.PendingCall, Output: "Actual file content"}}, s.Head)
	x.tool = false
	next, err := run(t, e, x, "tool-result", s.ID, p)
	require.NoError(t, err)
	require.NotEqual(t, first.Response.ID, next.Response.ID)
	require.Empty(t, x.calls[1].Request.PreviousResponseID)
	require.Contains(t, string(x.calls[1].Request.Input), "Actual file content")
	require.Equal(t, 2, x.charges)
	_, _, err = e.LoadSession(context.Background(), "another-key", s.ID)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestOperationMetadataIdentifiersAreBoundedBeforeStorage(t *testing.T) {
	store := newMem()
	e := New(store)
	for _, input := range []struct{ namespace, model, session string }{
		{"responses:", strings.Repeat("m", 257), ""},
		{"responses:", "model", strings.Repeat("s", 129)},
		{strings.Repeat("n", 257), "model", ""},
	} {
		_, _, err := e.Begin(context.Background(), owner(), input.namespace, "key", "body", input.model, input.session, DefaultPolicy())
		require.Error(t, err)
	}
	require.Empty(t, store.records, "invalid identifiers must not consume durable operation quota")
	_, _, err := e.Begin(context.Background(), owner(), strings.Repeat("n", 256), "key", "body", strings.Repeat("m", 256), strings.Repeat("s", 128), DefaultPolicy())
	require.NoError(t, err)
	require.NoError(t, e.Close(context.Background()))
}
func TestKnownUsageRecoveryDoesNotReplayProvider(t *testing.T) {
	store := newMem()
	e := New(store)
	x := &fakeExecutor{failSettle: true}
	p := parsed(t, "Keep constraints", "")
	_, err := run(t, e, x, "recover", "", p)
	require.Error(t, err)
	require.Equal(t, 1, x.callCount())
	// A new engine instance has no process-local knowledge of the original call.
	restarted := New(store)
	result, err := run(t, restarted, x, "recover", "", p)
	require.NoError(t, err)
	require.NotNil(t, result.Response)
	require.Equal(t, 1, x.callCount())
	require.Equal(t, 1, x.charges)
}
func TestPublishCommitRecoveryDoesNotAppendOutputTwice(t *testing.T) {
	store := newMem()
	e := New(store)
	x := &fakeExecutor{}
	p := parsed(t, "one task", "")
	failed := false
	store.fail = func(r *Record) bool {
		if !failed && r.Kind == "operation" && r.Status == "completed" {
			failed = true
			return true
		}
		return false
	}
	_, err := run(t, e, x, "publish", "", p)
	require.Error(t, err)
	require.Equal(t, 1, x.callCount())
	result, err := run(t, New(store), x, "publish", "", p)
	require.NoError(t, err)
	require.True(t, result.Replayed)
	s, _, err := e.LoadSession(context.Background(), owner().Scope, result.SessionID)
	require.NoError(t, err)
	require.Len(t, s.Active, 2)
	require.Equal(t, 1, x.charges)
}
func TestUnknownAttemptAndCancellationNeverDispatchAgain(t *testing.T) {
	e := New(newMem())
	x := &fakeExecutor{unknown: true}
	p := parsed(t, "task", "")
	_, err := run(t, e, x, "unknown", "", p)
	require.Error(t, err)
	require.Equal(t, 1, x.callCount())
	_, err = run(t, e, x, "unknown", "", p)
	require.Error(t, err)
	require.Equal(t, 1, x.callCount())
	require.Zero(t, x.charges)
	op, _, err := load[Operation](context.Background(), e.Store, owner().Scope, "operation", "op_"+cc.Digest([]string{"responses:", "unknown"}))
	require.NoError(t, err)
	require.NoError(t, e.Cancel(context.Background(), op.Scope, op.SessionID))
	_, err = run(t, e, x, "cancelled", op.SessionID, parsed(t, "continue", "resp_ctx_"+op.ID))
	require.Error(t, err)
	require.Equal(t, 1, x.callCount())
}
func TestLongToolSessionBeyondLegacyLimits(t *testing.T) {
	e := New(newMem())
	x := &fakeExecutor{tool: true, safe: 160000}
	result, err := run(t, e, x, "start", "", parsed(t, "Preserve the original public interface", ""))
	require.NoError(t, err)
	for n := 0; n < 270; n++ {
		s, _, err := e.LoadSession(context.Background(), owner().Scope, result.SessionID)
		require.NoError(t, err)
		result, err = run(t, e, x, fmt.Sprint("tool-", n), s.ID, parsed(t, []cc.Item{{Type: "function_call_output", CallID: s.PendingCall, Output: strings.Repeat("actual historical evidence ", 2000)}}, s.Head))
		require.NoError(t, err, "turn %d", n)
	}
	s, _, err := e.LoadSession(context.Background(), owner().Scope, result.SessionID)
	require.NoError(t, err)
	require.Equal(t, 271, s.Requests)
	require.Greater(t, s.HistoryBytes, int64(12<<20))
	require.Greater(t, s.Epoch, uint64(3))
	items, err := e.messages(context.Background(), s.Scope, s.Active)
	require.NoError(t, err)
	require.Contains(t, string(mustJSON(items)), "Preserve the original public interface")
	require.Greater(t, s.MaintenanceCostUSD, 0.0)
}

type currentAutoExecutor struct {
	*fakeExecutor
	enabled bool
}

func (x *currentAutoExecutor) AutoCompactionEnabled(context.Context) (bool, error) {
	return x.enabled, nil
}

func TestCurrentAccountAutoSwitchOverridesSessionSnapshot(t *testing.T) {
	e := New(newMem())
	s, op := seedSummary(t, e)
	x := &currentAutoExecutor{fakeExecutor: &fakeExecutor{safe: 12000}}
	err := e.WithOperation(context.Background(), op, func(ctx context.Context, op *Operation, or *Record, ol *Lease) error {
		ctx, l, release, err := e.lease(ctx, s.Scope, "session", s.ID)
		if err != nil {
			return err
		}
		defer release()
		s, sr, err := e.LoadSession(ctx, s.Scope, s.ID)
		if err != nil {
			return err
		}
		// Several completed items let a bounded summary select a prefix even
		// though the complete foreground input exceeds hard.
		s.Active = nil
		var blobs []Blob
		for _, item := range []cc.Item{
			{Type: "message", Role: "user", Content: mustJSON("Preserve exact public API")},
			{Type: "message", Role: "assistant", Content: mustJSON(strings.Repeat("a", 4000))},
			{Type: "message", Role: "assistant", Content: mustJSON(strings.Repeat("b", 4000))},
			{Type: "message", Role: "assistant", Content: mustJSON(strings.Repeat("c", 4000))},
			{Type: "message", Role: "user", Content: mustJSON("Continue")},
		} {
			ref, blob := e.itemBlob(s, item, true, false)
			s.Active = append(s.Active, ref)
			blobs = append(blobs, blob)
		}
		require.NoError(t, e.save(ctx, sr, s, s.Status, &l, blobs, &s.Policy))
		// A current disable must win over the original Auto=true policy
		// and must not dispatch any summary.
		require.True(t, s.Policy.Auto)
		_, err = e.prepare(ctx, s, sr, &l, op, or, ol, &s.Template, x)
		require.Error(t, err)
		require.Zero(t, x.callCount())
		// Turning it on works for an existing session even if its policy
		// was originally created while automatic compaction was disabled.
		s.Policy.Auto = false
		x.enabled = true
		req, err := e.prepare(ctx, s, sr, &l, op, or, ol, &s.Template, x)
		require.NoError(t, err)
		require.NotNil(t, req)
		require.Positive(t, x.callCount())
		require.Positive(t, s.Epoch)
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, e.Close(context.Background()))
}

func TestInternalCompactBelowTargetIsDurableNoop(t *testing.T) {
	for _, loseReceipt := range []bool{false, true} {
		t.Run(fmt.Sprint("lost-receipt-", loseReceipt), func(t *testing.T) {
			store := newMem()
			e := New(store)
			x := &fakeExecutor{}
			first, err := run(t, e, x, "first", "", parsed(t, "Keep the API", ""))
			require.NoError(t, err)
			s, sr, err := e.LoadSession(context.Background(), owner().Scope, first.SessionID)
			require.NoError(t, err)
			revision := sr.Revision
			op, _, err := e.Begin(context.Background(), owner(), "internal:"+s.ID, "noop", "body", s.Model, s.ID, s.Policy)
			require.NoError(t, err)
			failed := false
			store.fail = func(r *Record) bool {
				if loseReceipt && !failed && r.Kind == "operation" && r.ID == op.ID && r.Status == "completed" {
					failed = true
					return true
				}
				return false
			}
			compact := func() (*Job, error) {
				var job *Job
				err := e.WithOperation(context.Background(), op, func(ctx context.Context, op *Operation, r *Record, l *Lease) error {
					var err error
					job, err = e.Internal(ctx, op, r, l, revision, x)
					return err
				})
				return job, err
			}
			job, err := compact()
			if loseReceipt {
				require.Error(t, err)
				_, err = run(t, e, x, "next", s.ID, parsed(t, "Continue", first.Response.ID))
				require.NoError(t, err)
				job, err = compact()
			}
			require.NoError(t, err)
			require.Equal(t, "no_op", job.State)
			require.Equal(t, job.Before, job.After)
			require.True(t, job.Accounted)
			replayed, err := compact()
			require.NoError(t, err)
			require.Equal(t, job.ID, replayed.ID)
			expected := 1
			if loseReceipt {
				expected++
			}
			require.Equal(t, expected, x.callCount(), "no extra provider calls for internal compact")
			require.Equal(t, expected, x.charges)
			require.NoError(t, e.Close(context.Background()))
		})
	}
}

func TestManualSummaryReservationHasItsOwnOperationIdentity(t *testing.T) {
	e := New(newMem())
	s, op := seedSummary(t, e)
	x := &fakeExecutor{safe: 20000}
	observed := false
	x.beforeReturn = func(call Call) {
		if strings.HasPrefix(call.Purpose, "compaction") {
			stored, _, err := e.LoadSession(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			require.Empty(t, stored.OperationID, "manual compact does not claim a foreground turn")
			require.Equal(t, op.ID, stored.ReservationOperationID)
			require.Positive(t, stored.ReservedBytes)
			observed = true
		}
	}
	_, sr, err := e.LoadSession(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	err = e.WithOperation(context.Background(), op, func(ctx context.Context, op *Operation, r *Record, l *Lease) error {
		job, err := e.Internal(ctx, op, r, l, sr.Revision, x)
		if err == nil {
			require.Equal(t, "applied", job.State)
		}
		return err
	})
	require.NoError(t, err)
	require.True(t, observed)
	require.NoError(t, e.Close(context.Background()))
}

func TestManualSummaryPreparationFailureReleasesItsReservation(t *testing.T) {
	e := New(newMem())
	defer func() { require.NoError(t, e.Close(context.Background())) }()
	s, _ := seedSummary(t, e)
	x := &fakeExecutor{safe: 20000}
	limited := s.Policy
	limited.MaxTurnCostUSD = .0005 // Session maintenance admits the .001 estimate.
	op, _, err := e.Begin(context.Background(), owner(), "internal:"+s.ID, "over-turn-cap", "first", s.Model, s.ID, limited)
	require.NoError(t, err)
	compact := func(op *Operation) error {
		_, sr, err := e.LoadSession(context.Background(), s.Scope, s.ID)
		if err != nil {
			return err
		}
		return e.WithOperation(context.Background(), op, func(ctx context.Context, op *Operation, r *Record, l *Lease) error {
			_, err := e.Internal(ctx, op, r, l, sr.Revision, x)
			return err
		})
	}
	require.ErrorContains(t, compact(op), "budget")
	stored, _, err := e.LoadSession(context.Background(), s.Scope, s.ID)
	require.NoError(t, err)
	require.Zero(t, stored.ReservedBytes)
	require.Empty(t, stored.ReservationOperationID)
	require.Zero(t, x.callCount())
	fresh, _, err := e.Begin(context.Background(), owner(), "internal:"+s.ID, "after-preparation-failure", "next", s.Model, s.ID, s.Policy)
	require.NoError(t, err)
	x.beforeReturn = func(Call) {
		current, _, err := e.LoadSession(context.Background(), s.Scope, s.ID)
		require.NoError(t, err)
		require.Equal(t, fresh.ID, current.ReservationOperationID)
	}
	require.NoError(t, compact(fresh))
	require.Equal(t, 1, x.callCount())
	require.Equal(t, 1, x.charges)
}

func TestInternalTerminalRecoveryReleasesOnlyItsReservation(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy-", legacy), func(t *testing.T) {
			store := newMem()
			e := New(store)
			defer func() { require.NoError(t, e.Close(context.Background())) }()
			s, op := seedSummary(t, e)
			x := &fakeExecutor{safe: 20000}
			_, sr, err := e.LoadSession(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			revision := sr.Revision
			failed := false
			store.fail = func(r *Record) bool {
				if !failed && r.Kind == "operation" && r.ID == op.ID && r.Status == "completed" {
					failed = true
					return true
				}
				return false
			}
			compact := func() error {
				return e.WithOperation(context.Background(), op, func(ctx context.Context, op *Operation, r *Record, l *Lease) error {
					job, err := e.Internal(ctx, op, r, l, revision, x)
					if err == nil {
						require.Equal(t, "applied", job.State)
					}
					return err
				})
			}
			require.Error(t, compact(), "interrupt after the applied job but before the receipt")
			s, sr, err = e.LoadSession(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			require.NotEmpty(t, s.AppliedJobID)
			require.Zero(t, s.ReservedBytes, "application releases the reservation atomically")
			require.Empty(t, s.ReservationOperationID)
			// Reproduce an older journal that committed application separately
			// from releasing its manual reservation.
			s.ReservedBytes, s.ReservationOperationID = 123456, op.ID
			if legacy {
				s.ReservationOperationID = ""
			}
			require.NoError(t, e.save(context.Background(), sr, s, s.Status, nil, nil, &s.Policy))
			require.NoError(t, compact())
			s, sr, err = e.LoadSession(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			require.Zero(t, s.ReservedBytes)
			require.Empty(t, s.ReservationOperationID)
			s.OperationID, s.ReservationOperationID, s.ReservedBytes = "later-turn", "later-turn", 654321
			require.NoError(t, e.save(context.Background(), sr, s, s.Status, nil, nil, &s.Policy))
			require.NoError(t, compact(), "replay must not release another turn's reservation")
			s, _, err = e.LoadSession(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			require.Equal(t, int64(654321), s.ReservedBytes)
			require.Equal(t, "later-turn", s.ReservationOperationID)
			// A legacy manual reservation belongs to its current JobID, even
			// while AppliedJobID still references this older completed summary.
			s, sr, err = e.LoadSession(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			s.OperationID, s.ReservationOperationID, s.JobID = "", "", "later-manual"
			require.NoError(t, e.save(context.Background(), sr, s, s.Status, nil, nil, &s.Policy))
			require.NoError(t, compact())
			s, _, err = e.LoadSession(context.Background(), s.Scope, s.ID)
			require.NoError(t, err)
			require.Equal(t, int64(654321), s.ReservedBytes)
			require.Equal(t, 1, x.callCount())
			require.Equal(t, 1, x.charges)
		})
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func TestStatelessNoopAndKnownResultReplay(t *testing.T) {
	e := New(newMem())
	x := &fakeExecutor{safe: 20000}
	body := mustJSON(map[string]any{"model": "concrete", "input": []cc.Item{{Type: "message", Role: "user", Content: json.RawMessage(`"Goal"`)}, {Type: "message", Role: "assistant", Content: mustJSON(strings.Repeat("old evidence ", 800))}, {Type: "message", Role: "user", Content: json.RawMessage(`"Current constraint"`)}}})
	p, err := Parse(body, true)
	require.NoError(t, err)
	op, _, err := e.Begin(context.Background(), owner(), "stateless", "compact", cc.Digest(body), "concrete", "", DefaultPolicy())
	require.NoError(t, err)
	var result *CompactResult
	err = e.WithOperation(context.Background(), op, func(ctx context.Context, op *Operation, r *Record, l *Lease) error {
		result, err = e.Stateless(ctx, op, r, l, p, 5000, x)
		return err
	})
	require.NoError(t, err)
	require.Equal(t, "compacted", result.Status)
	require.LessOrEqual(t, result.After, 5000)
	require.Equal(t, 1, x.callCount())
	require.Contains(t, string(mustJSON(result.Input)), "Current constraint")
	err = e.WithOperation(context.Background(), op, func(ctx context.Context, op *Operation, r *Record, l *Lease) error {
		result, err = e.Stateless(ctx, op, r, l, p, 5000, x)
		return err
	})
	require.NoError(t, err)
	require.Equal(t, 1, x.callCount())
	noop, _, err := e.Begin(context.Background(), owner(), "stateless", "noop", "noop", "concrete", "", DefaultPolicy())
	require.NoError(t, err)
	p, err = Parse([]byte(`{"model":"concrete","input":"short input"}`), true)
	require.NoError(t, err)
	err = e.WithOperation(context.Background(), noop, func(ctx context.Context, op *Operation, r *Record, l *Lease) error {
		result, err = e.Stateless(ctx, op, r, l, p, 5000, x)
		return err
	})
	require.NoError(t, err)
	require.Equal(t, "no_op", result.Status)
	require.Equal(t, 1, x.callCount())
}

func TestStatelessRecoveryStopsBeforeFreshInference(t *testing.T) {
	e := New(newMem())
	defer func() { require.NoError(t, e.Close(context.Background())) }()
	x := &fakeExecutor{safe: 20000}
	body := mustJSON(map[string]any{"model": "concrete", "input": []cc.Item{{Type: "message", Role: "user", Content: mustJSON("Goal")}, {Type: "message", Role: "assistant", Content: mustJSON(strings.Repeat("old evidence ", 800))}, {Type: "message", Role: "user", Content: mustJSON("Current constraint")}}})
	p, err := Parse(body, true)
	require.NoError(t, err)
	op, _, err := e.Begin(context.Background(), owner(), "stateless", "recover", cc.Digest(body), "concrete", "", DefaultPolicy())
	require.NoError(t, err)
	err = e.WithOperation(context.Background(), op, func(ctx context.Context, op *Operation, r *Record, l *Lease) error {
		result, err := e.RecoverStateless(ctx, op, r, l, p, 5000, x)
		require.NoError(t, err)
		require.Nil(t, result, "missing summary returns to fresh admission")
		require.Zero(t, x.callCount())
		require.Zero(t, x.charges)
		require.Empty(t, op.Attempts, "recovery must not journal or dispatch another call")
		result, err = e.Stateless(ctx, op, r, l, p, 5000, x)
		require.NoError(t, err)
		require.Equal(t, "compacted", result.Status)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, x.callCount())
	require.Equal(t, 1, x.charges)
}
func TestManagedParserRejectsMultimodalAndUnpairedToolData(t *testing.T) {
	for _, body := range []string{`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,a"}]}]}`, `{"model":"m","input":[{"type":"function_call_output","call_id":"unknown","output":"x"}]}`, `{"model":"m","input":[{"type":"function_call","call_id":"c","name":"t","arguments":"{}"}]}`, `{"model":"m","input":"x","store":true}`} {
		_, err := Parse([]byte(body), true)
		require.Error(t, err, body)
	}
}

func seedSummary(t *testing.T, e *Engine) (*Session, *Operation) {
	t.Helper()
	ctx := context.Background()
	p := DefaultPolicy()
	template := parsed(t, "goal", "").Request
	n := 1024
	template.MaxOutputTokens = &n
	template.Input = nil
	s := &Session{Owner: owner(), ID: "ctx_seed", Schema: 2, Model: "concrete", Template: *template, Policy: p, Contract: contract(template), Status: "ready", ExpiresAt: time.Now().Add(time.Hour)}
	sr := record(s.Owner, "session", s.ID, s.Status, s, s.ExpiresAt)
	_, err := e.Store.Create(ctx, sr, &p)
	require.NoError(t, err)
	l, err := e.Store.Acquire(ctx, s.Scope, "session", s.ID, "seed", time.Minute)
	require.NoError(t, err)
	var blobs []Blob
	for _, i := range []cc.Item{{Type: "message", Role: "user", Content: mustJSON("Keep the exact public interface")}, {Type: "message", Role: "assistant", Content: mustJSON(strings.Repeat("observed historical evidence ", 350))}, {Type: "message", Role: "user", Content: mustJSON("Continue the current task")}} {
		ref, b := e.itemBlob(s, i, true, false)
		s.Active = append(s.Active, ref)
		s.HistoryBytes += int64(ref.Bytes)
		blobs = append(blobs, b)
	}
	require.NoError(t, e.save(ctx, sr, s, s.Status, &l, blobs, &p))
	require.NoError(t, e.Store.Release(ctx, l))
	op, _, err := e.Begin(ctx, owner(), "internal:"+s.ID, "summary", "same", "concrete", s.ID, p)
	require.NoError(t, err)
	return s, op
}
func launchSummary(t *testing.T, e *Engine, op *Operation, x Executor, appendTail bool) string {
	t.Helper()
	jobID := ""
	err := e.WithOperation(context.Background(), op, func(ctx context.Context, op *Operation, or *Record, ol *Lease) error {
		s, sr, err := e.LoadSession(ctx, op.Scope, op.SessionID)
		if err != nil {
			return err
		}
		ctx, l, release, err := e.lease(ctx, s.Scope, "session", s.ID)
		if err != nil {
			return err
		}
		defer release()
		started, err := e.startSummary(ctx, s, sr, &l, op, or, ol, &s.Template, x, true, "auto", 11999)
		if err != nil {
			return err
		}
		if !started {
			return errors.New("summary did not start")
		}
		jobID = s.JobID
		if appendTail {
			ref, b := e.itemBlob(s, cc.Item{Type: "message", Role: "user", Content: mustJSON("Exact appended suffix must survive")}, true, false)
			s.Active = append(s.Active, ref)
			s.HistoryBytes += int64(ref.Bytes)
			return e.save(ctx, sr, s, s.Status, &l, []Blob{b}, &s.Policy)
		}
		return nil
	})
	require.NoError(t, err)
	return jobID
}
func waitReady(t *testing.T, e *Engine, job string) {
	t.Helper()
	require.Eventually(t, func() bool {
		j, _, err := load[Job](context.Background(), e.Store, owner().Scope, "job", job)
		if err != nil || j.State != "ready" {
			return false
		}
		a, _, err := load[Attempt](context.Background(), e.Store, owner().Scope, "attempt", j.AttemptID)
		return err == nil && a.Settled
	}, 3*time.Second, 5*time.Millisecond)
}
func TestAsyncSummaryPreservesAppendedTailAndBillsOnce(t *testing.T) {
	e := New(newMem())
	x := &fakeExecutor{safe: 20000, allowAux: true, started: make(chan struct{}), finish: make(chan struct{})}
	s, op := seedSummary(t, e)
	job := launchSummary(t, e, op, x, true)
	select {
	case <-x.started:
	case <-time.After(time.Second):
		t.Fatal("summary did not dispatch")
	}
	close(x.finish)
	waitReady(t, e, job)
	ctx, l, release, err := e.lease(context.Background(), s.Scope, "session", s.ID)
	require.NoError(t, err)
	defer release()
	s, sr, err := e.LoadSession(ctx, s.Scope, s.ID)
	require.NoError(t, err)
	require.NoError(t, e.collect(ctx, s, sr, &l, x))
	applied, err := e.apply(ctx, s, sr, &l, &s.Template, x)
	require.NoError(t, err)
	require.True(t, applied)
	items, err := e.messages(ctx, s.Scope, s.Active)
	require.NoError(t, err)
	wire := string(mustJSON(items))
	require.Contains(t, wire, "Keep the exact public interface")
	require.Contains(t, wire, "Exact appended suffix must survive")
	require.EqualValues(t, 1, s.Epoch)
	require.NoError(t, e.collect(ctx, s, sr, &l, x))
	x.mu.Lock()
	charges := x.charges
	x.mu.Unlock()
	require.Equal(t, 1, charges)
	require.NoError(t, e.Close(context.Background()))
}

func TestBackgroundReservationSurvivesForegroundCompletion(t *testing.T) {
	e := New(newMem())
	x := &fakeExecutor{safe: 20000, allowAux: true, started: make(chan struct{}), finish: make(chan struct{})}
	s, op := seedSummary(t, e)
	id := launchSummary(t, e, op, x, false)
	<-x.started
	j, _, err := load[Job](context.Background(), e.Store, s.Scope, "job", id)
	require.NoError(t, err)
	require.Positive(t, j.AllocatedBytes)
	require.Equal(t, j.AllocatedBytes, j.ReservedBytes)
	a, _, err := load[Attempt](context.Background(), e.Store, s.Scope, "attempt", j.AttemptID)
	require.NoError(t, err)
	require.Equal(t, id, a.ReservationID)
	ctx, l, release, err := e.lease(context.Background(), s.Scope, "session", s.ID)
	require.NoError(t, err)
	s, sr, err := e.LoadSession(ctx, s.Scope, s.ID)
	require.NoError(t, err)
	s.ReservedBytes, s.ReservationOperationID = 0, ""
	require.NoError(t, e.save(ctx, sr, s, s.Status, &l, nil, &s.Policy))
	release()
	j, _, err = load[Job](context.Background(), e.Store, s.Scope, "job", id)
	require.NoError(t, err)
	require.Positive(t, j.ReservedBytes, "foreground publication cannot clear a background promise")
	close(x.finish)
	waitReady(t, e, id)
	j, _, err = load[Job](context.Background(), e.Store, s.Scope, "job", id)
	require.NoError(t, err)
	require.Zero(t, j.ReservedBytes)
	require.Positive(t, j.AllocatedBytes, "the immutable partition cannot be returned to the foreground")
	a, _, err = load[Attempt](context.Background(), e.Store, s.Scope, "attempt", j.AttemptID)
	require.NoError(t, err)
	_, err = e.Store.Blob(context.Background(), s.Scope, a.ResponseRef)
	require.NoError(t, err)
	require.NoError(t, e.Close(context.Background()))
}
func TestCancelledLateSummaryIsSettledAndNeverApplied(t *testing.T) {
	e := New(newMem())
	x := &fakeExecutor{safe: 20000, allowAux: true, late: true, started: make(chan struct{}), finish: make(chan struct{})}
	s, op := seedSummary(t, e)
	job := launchSummary(t, e, op, x, false)
	select {
	case <-x.started:
	case <-time.After(time.Second):
		t.Fatal("summary did not dispatch")
	}
	require.NoError(t, e.Cancel(context.Background(), s.Scope, s.ID))
	close(x.finish)
	waitReady(t, e, job)
	ctx, l, release, err := e.lease(context.Background(), s.Scope, "session", s.ID)
	require.NoError(t, err)
	defer release()
	s, sr, err := e.LoadSession(ctx, s.Scope, s.ID)
	require.NoError(t, err)
	// Even a valid summary with known paid usage cannot commit to a cancelled lane.
	j, jr, err := load[Job](ctx, e.Store, s.Scope, "job", job)
	require.NoError(t, err)
	j.Accounted = true
	require.NoError(t, e.save(ctx, jr, j, j.State, nil, nil, nil))
	applied, err := e.apply(ctx, s, sr, &l, &s.Template, x)
	require.Error(t, err)
	require.False(t, applied)
	stored, _, err := e.LoadSession(ctx, s.Scope, s.ID)
	require.NoError(t, err)
	require.Zero(t, stored.Epoch)
	require.Equal(t, "cancelled", stored.Status)
	x.mu.Lock()
	charges := x.charges
	x.mu.Unlock()
	require.Equal(t, 1, charges)
	require.NoError(t, e.Close(context.Background()))
}
func TestDisconnectedOperationKeepsLeaseContextAndShutdownStopsNewWork(t *testing.T) {
	e := New(newMem())
	op, _, err := e.Begin(context.Background(), owner(), "stateless", "disconnect", "f", "concrete", "", DefaultPolicy())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, e.WithOperation(ctx, op, func(ctx context.Context, _ *Operation, _ *Record, _ *Lease) error { return ctx.Err() }))
	require.NoError(t, e.Close(context.Background()))
	require.Error(t, e.WithOperation(context.Background(), op, func(context.Context, *Operation, *Record, *Lease) error {
		t.Fatal("work entered after shutdown")
		return nil
	}))
}

func TestPublishedResponseRecoversAfterNewHead(t *testing.T) {
	store := newMem()
	e := New(store)
	x := &fakeExecutor{}
	p := parsed(t, "first task", "")
	failed := false
	store.fail = func(r *Record) bool {
		if !failed && r.Kind == "operation" && r.Status == "completed" {
			failed = true
			return true
		}
		return false
	}
	_, err := run(t, e, x, "first", "", p)
	require.Error(t, err)
	op, _, err := load[Operation](context.Background(), store, owner().Scope, "operation", "op_"+cc.Digest([]string{"responses:", "first"}))
	require.NoError(t, err)
	s, _, err := e.LoadSession(context.Background(), owner().Scope, op.SessionID)
	require.NoError(t, err)
	second, err := run(t, e, x, "second", s.ID, parsed(t, "new input", s.Head))
	require.NoError(t, err)
	require.NotEqual(t, "resp_ctx_"+op.ID, second.Response.ID)
	recovered, err := run(t, New(store), x, "first", "", p)
	require.NoError(t, err)
	require.True(t, recovered.Replayed)
	require.Equal(t, "resp_ctx_"+op.ID, recovered.Response.ID)
	require.Equal(t, 2, x.callCount())
}

func TestLateKnownResultSurvivesRecoveryMarkingUnknown(t *testing.T) {
	e := New(newMem())
	x := &fakeExecutor{}
	x.beforeReturn = func(c Call) {
		a, r, err := load[Attempt](context.Background(), e.Store, owner().Scope, "attempt", c.ID)
		require.NoError(t, err)
		a.Deadline = time.Now().Add(-time.Second)
		require.NoError(t, e.save(context.Background(), r, a, a.State, nil, nil, nil))
		require.NoError(t, e.ReconcileAttempt(context.Background(), a.Scope, a.ID, nil))
		a, _, err = load[Attempt](context.Background(), e.Store, a.Scope, "attempt", a.ID)
		require.NoError(t, err)
		require.Equal(t, "unknown", a.State)
	}
	result, err := run(t, e, x, "late-known", "", parsed(t, "keep original constraint", ""))
	require.NoError(t, err)
	require.NotNil(t, result.Response)
	require.Equal(t, 1, x.callCount())
	require.Equal(t, 1, x.charges)
}

func TestBilledUsageReceiptFailureRecoversWithoutDuplicateCharge(t *testing.T) {
	store := newMem()
	e := New(store)
	x := &fakeExecutor{}
	failed := false
	store.fail = func(r *Record) bool {
		if r.Kind == "attempt" {
			a, _ := decode[Attempt](r)
			if !failed && a.Settled {
				failed = true
				return true
			}
		}
		return false
	}
	p := parsed(t, "keep all original constraints", "")
	_, err := run(t, e, x, "bill-receipt", "", p)
	require.Error(t, err)
	require.Equal(t, 1, x.charges)
	result, err := run(t, New(store), x, "bill-receipt", "", p)
	require.NoError(t, err)
	require.NotNil(t, result.Response)
	require.Equal(t, 1, x.callCount())
	require.Equal(t, 1, x.charges)
}
func TestAppliedSessionCommitRepairsJobWithoutSecondEpoch(t *testing.T) {
	store := newMem()
	e := New(store)
	x := &fakeExecutor{safe: 20000, allowAux: true, started: make(chan struct{}), finish: make(chan struct{})}
	s, op := seedSummary(t, e)
	job := launchSummary(t, e, op, x, false)
	<-x.started
	close(x.finish)
	waitReady(t, e, job)
	ctx, l, release, err := e.lease(context.Background(), s.Scope, "session", s.ID)
	require.NoError(t, err)
	defer release()
	s, sr, err := e.LoadSession(ctx, s.Scope, s.ID)
	require.NoError(t, err)
	require.NoError(t, e.collect(ctx, s, sr, &l, x))
	failed := false
	store.fail = func(r *Record) bool {
		if !failed && r.Kind == "job" && r.Status == "applied" {
			failed = true
			return true
		}
		return false
	}
	applied, err := e.apply(ctx, s, sr, &l, &s.Template, x)
	require.True(t, applied)
	require.Error(t, err)
	j, _, err := New(store).LoadJob(ctx, s.Scope, job)
	require.NoError(t, err)
	require.Equal(t, "applied", j.State)
	require.Positive(t, j.After)
	applied, err = e.apply(ctx, s, sr, &l, &s.Template, x)
	require.NoError(t, err)
	require.False(t, applied)
	require.EqualValues(t, 1, s.Epoch)
	require.NoError(t, e.Close(context.Background()))
}
func TestOversizedCurrentToolGroupDoesNotPayForImpossibleSummary(t *testing.T) {
	e := New(newMem())
	x := &fakeExecutor{safe: 20000}
	s, op := seedSummary(t, e)
	err := e.WithOperation(context.Background(), op, func(ctx context.Context, op *Operation, or *Record, ol *Lease) error {
		s, sr, err := e.LoadSession(ctx, s.Scope, s.ID)
		if err != nil {
			return err
		}
		ctx, l, release, err := e.lease(ctx, s.Scope, "session", s.ID)
		if err != nil {
			return err
		}
		defer release()
		var blobs []Blob
		for _, item := range []cc.Item{{Type: "function_call", CallID: "last", Name: "read", Arguments: `{}`}, {Type: "function_call_output", CallID: "last", Output: strings.Repeat("big current evidence ", 900)}} {
			ref, b := e.itemBlob(s, item, true, false)
			s.Active = append(s.Active, ref)
			s.HistoryBytes += int64(ref.Bytes)
			blobs = append(blobs, b)
		}
		if err = e.save(ctx, sr, s, s.Status, &l, blobs, &s.Policy); err != nil {
			return err
		}
		_, err = e.prepare(ctx, s, sr, &l, op, or, ol, &s.Template, x)
		return err
	})
	require.Error(t, err)
	require.Equal(t, 0, x.callCount())
	require.Zero(t, x.charges)
}
func TestFailedStatelessCompactRetainsInspectableUsage(t *testing.T) {
	e := New(newMem())
	x := &fakeExecutor{safe: 20000, summaryText: "invalid structured summary"}
	body := mustJSON(map[string]any{"model": "concrete", "input": []cc.Item{{Type: "message", Role: "user", Content: mustJSON("Goal")}, {Type: "message", Role: "assistant", Content: mustJSON(strings.Repeat("old evidence ", 800))}, {Type: "message", Role: "user", Content: mustJSON("Current constraint")}}})
	p, err := Parse(body, true)
	require.NoError(t, err)
	op, _, err := e.Begin(context.Background(), owner(), "stateless", "bad-summary", "f", "concrete", "", DefaultPolicy())
	require.NoError(t, err)
	err = e.WithOperation(context.Background(), op, func(ctx context.Context, op *Operation, r *Record, l *Lease) error {
		_, err := e.Stateless(ctx, op, r, l, p, 5000, x)
		return err
	})
	require.Error(t, err)
	j, _, err := e.LoadJob(context.Background(), op.Scope, "cmp_"+op.ID)
	require.NoError(t, err)
	require.Equal(t, "failed", j.State)
	require.True(t, j.Accounted)
	require.NotNil(t, j.Usage)
	require.NotEmpty(t, j.AttemptID)
	require.Equal(t, 1, x.charges)
}

func TestConcreteOutputCapBoundsDefaultsAndIndependentSummary(t *testing.T) {
	e := New(newMem())
	x := &fakeExecutor{outputCap: 256, safe: 20000}
	policy := DefaultPolicy()
	policy.MaxOutputTokens = 64
	require.Equal(t, 256, summaryLimit(policy, x), "a small foreground policy must not truncate the independent summary budget")
	first, err := run(t, e, x, "cap-default", "", parsed(t, "user constraint", ""))
	require.NoError(t, err)
	s, _, err := e.LoadSession(context.Background(), owner().Scope, first.SessionID)
	require.NoError(t, err)
	require.Equal(t, 256, *s.Template.MaxOutputTokens)
	p := parsed(t, "another task", "")
	n := 257
	p.Request.MaxOutputTokens = &n
	_, err = run(t, e, x, "cap-reject", "", p)
	require.Error(t, err)
	require.Equal(t, 1, x.callCount())
	summaryEngine := New(newMem())
	x = &fakeExecutor{outputCap: 256, safe: 20000, allowAux: true, started: make(chan struct{}), finish: make(chan struct{})}
	_, op := seedSummary(t, summaryEngine)
	job := launchSummary(t, summaryEngine, op, x, false)
	<-x.started
	close(x.finish)
	waitReady(t, summaryEngine, job)
	x.mu.Lock()
	tokens := *x.calls[0].Request.MaxOutputTokens
	x.mu.Unlock()
	require.Equal(t, 256, tokens, "summary budget follows concrete cap and is independent of foreground output length")
	require.NoError(t, summaryEngine.Close(context.Background()))
}

func TestSessionExpiryAfterKnownUsageDoesNotPublishNewHead(t *testing.T) {
	e := New(newMem())
	x := &fakeExecutor{}
	now := time.Now()
	e.Now = func() time.Time { return now }
	x.beforeReturn = func(Call) { now = now.Add(25 * time.Hour) }
	_, err := run(t, e, x, "expires-while-paying", "", parsed(t, "user constraint", ""))
	require.Error(t, err)
	require.Equal(t, "context_expired", typed(err).Code)
	require.Equal(t, 1, x.charges)
	op, _, err := load[Operation](context.Background(), e.Store, owner().Scope, "operation", "op_"+cc.Digest([]string{"responses:", "expires-while-paying"}))
	require.NoError(t, err)
	s, _, err := e.LoadSession(context.Background(), op.Scope, op.SessionID)
	require.NoError(t, err)
	require.Empty(t, s.Head)
	require.NotEmpty(t, s.OperationID)
	require.Equal(t, 1, x.callCount())
}
