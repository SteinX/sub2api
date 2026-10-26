//go:build devine2e

package devine2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// This test-runner fence is shared by every public admission in a batch. It
// does not purport to recall a request already sent to a remote provider.
type journeyFence struct {
	mu            sync.Mutex
	changed       chan struct{}
	stopped       bool
	active        bool
	phase         bool
	terminal      bool
	pending       map[string]bool
	acks          map[string]bool
	seenCalls     map[string]bool
	acknowledged  map[string]bool
	observerEpoch int
	cancel        context.CancelFunc
	stopOwned     func() bool
	budget        journeyBudget
	first         map[string]any
	journal       []map[string]any
	forwarded     int
	denied        int
	backendDead   bool
	deadline      *time.Timer
	stopDone      chan struct{}
}

func newJourneyFence(app *application, budget journeyBudget) *journeyFence {
	f := &journeyFence{changed: make(chan struct{}), pending: map[string]bool{}, acks: map[string]bool{}, seenCalls: map[string]bool{}, acknowledged: map[string]bool{}, budget: budget, stopDone: make(chan struct{})}
	if app != nil && app.command != nil {
		// Capture exactly this owned process, never a PID file or a later restart.
		process, done := app.command.Process, app.done
		f.stopOwned = func() bool {
			_ = process.Kill()
			select {
			case <-done:
				return true
			case <-time.After(3 * time.Second):
				return false
			}
		}
	}
	if _, authorized := budget.(*authorizedJourneyBudget); authorized {
		var state struct {
			Deadline float64 `json:"deadline_unix"`
		}
		data, err := os.ReadFile(os.Getenv("DEVIN_JOURNEY_RUN_LEDGER"))
		if err != nil || json.Unmarshal(data, &state) != nil || state.Deadline <= 0 {
			f.stop("deadline", time.Now().UnixNano())
		} else {
			f.installDeadline(time.Unix(0, int64(state.Deadline*1e9)))
		}
	}
	return f
}

func (f *journeyFence) installDeadline(deadline time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// An already expired timer may run immediately. Publish it under the same
	// mutex used by stop so the callback cannot race this assignment.
	f.deadline = time.AfterFunc(time.Until(deadline), func() { f.stop("deadline", time.Now().UnixNano()) })
}

func (f *journeyFence) notify() { close(f.changed); f.changed = make(chan struct{}) }

func (f *journeyFence) stop(reason string, observed int64) {
	switch reason {
	case "tool_error", "permission_rejection", "test_failed", "reader_error", "parse_error", "client_exit", "protocol_error", "transport_uncertain", "control_lost", "supervisor_failure", "deadline", "missing_terminal", "source_or_oracle_failure":
	default:
		reason = "protocol_error"
	}
	f.mu.Lock()
	if f.stopped {
		done := f.stopDone
		f.mu.Unlock()
		<-done
		return
	}
	f.stopped = true
	if f.deadline != nil {
		f.deadline.Stop()
	}
	f.first = map[string]any{"reason": reason, "observed_at_ns": observed, "gate_closed_at_ns": time.Now().UnixNano(), "public_admitted_at_stop": f.forwarded}
	f.journal = append(f.journal, map[string]any{"action": "stop", "at_ns": time.Now().UnixNano(), "reason": reason})
	cancel, kill := f.cancel, f.stopOwned
	f.notify()
	f.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	dead := false
	if kill != nil {
		dead = kill()
	}
	deadAt := time.Now().UnixNano()
	var persistErr error
	var persistence any
	if budget, ok := f.budget.(interface{ Stop(string, int64) error }); ok {
		persistErr = budget.Stop(reason, observed)
		persistence = persistErr == nil
	}
	f.mu.Lock()
	f.backendDead = dead
	f.first["owned_backend_death_verified"] = dead
	if dead {
		f.first["owned_backend_dead_at_ns"] = deadAt
	} else {
		f.first["owned_backend_dead_at_ns"] = nil
	}
	f.first["stop_persistence_verified"] = persistence
	f.first["stop_actions_finished_at_ns"] = time.Now().UnixNano()
	close(f.stopDone)
	f.mu.Unlock()
}

func (f *journeyFence) acquire(ctx context.Context) (context.Context, context.CancelFunc, error) {
	for {
		f.mu.Lock()
		if f.stopped {
			f.denied++
			f.mu.Unlock()
			return nil, nil, errors.New("batch stopped")
		}
		if f.phase && !f.active && len(f.pending) == 0 {
			f.active = true
			f.terminal = false
			child, cancel := context.WithCancel(ctx)
			f.cancel = cancel
			f.mu.Unlock()
			return child, cancel, nil
		}
		changed := f.changed
		f.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-changed:
		}
	}
}

// Called immediately before forwarding while the shared active slot is held.
func (f *journeyFence) admit() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		f.denied++
		return false
	}
	f.forwarded++
	f.journal = append(f.journal, map[string]any{"action": "forward", "at_ns": time.Now().UnixNano(), "ordinal": f.forwarded})
	return true
}

func callHash(id string) string {
	value := sha256.Sum256([]byte(id))
	return hex.EncodeToString(value[:])
}

// Parse IDs from the actual public response, not the fixture, user prompt,
// local part ID or tool output. Arguments/content are never retained here.
func journeyResponseCalls(data []byte, stream bool) ([]string, error) {
	if !validJourneyCompletion(data, stream) {
		return nil, errors.New("incomplete response")
	}
	ids := map[int]string{}
	consume := func(raw []byte) error {
		var v struct {
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Tools []struct {
						Index int    `json:"index"`
						ID    string `json:"id"`
					} `json:"tool_calls"`
				} `json:"delta"`
				Message struct {
					Tools []struct {
						ID string `json:"id"`
					} `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &v); err != nil {
			return err
		}
		for _, c := range v.Choices {
			if c.Index != 0 {
				return errors.New("multiple choices unsupported")
			}
			if stream {
				for _, tool := range c.Delta.Tools {
					if tool.Index < 0 || tool.Index >= 64 {
						return errors.New("tool index out of bounds")
					}
					if tool.ID != "" {
						if ids[tool.Index] != "" && ids[tool.Index] != tool.ID {
							return errors.New("tool identity changed")
						}
						ids[tool.Index] = tool.ID
					} else if _, ok := ids[tool.Index]; !ok {
						ids[tool.Index] = ""
					}
				}
			} else {
				for index, tool := range c.Message.Tools {
					ids[index] = tool.ID
				}
			}
		}
		return nil
	}
	if !stream {
		if err := consume(data); err != nil {
			return nil, err
		}
	} else {
		for _, line := range bytes.Split(data, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			raw := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if bytes.Equal(raw, []byte("[DONE]")) {
				continue
			}
			if err := consume(raw); err != nil {
				return nil, err
			}
		}
	}
	seen := map[string]bool{}
	calls := []string{}
	for index := 0; index < len(ids); index++ {
		id := ids[index]
		if id == "" || len(id) > 1024 || seen[id] {
			return nil, errors.New("missing or duplicate tool identity")
		}
		seen[id] = true
		calls = append(calls, callHash(id))
	}
	return calls, nil
}

func (f *journeyFence) finishResponse(record *agentRequest) {
	calls, err := journeyResponseCalls(nil, false)
	if record.response != nil && record.response.eof && record.Status < 400 {
		calls, err = journeyResponseCalls(record.response.data, record.Stream)
	}
	if err != nil {
		f.stop("transport_uncertain", time.Now().UnixNano())
	}
	f.mu.Lock()
	if !f.stopped {
		for _, id := range calls {
			if f.seenCalls[id] {
				err = errors.New("reused public tool identity")
			}
			f.seenCalls[id] = true
			if !f.acks[id] {
				f.pending[id] = true
			} else {
				f.acknowledged[id] = true
			}
		}
		// A normal no-tool response may be followed by compact/main requests in
		// one CLI operation. It needs no tool ACK. CLI errors still close the
		// fence when observed; protocol uncertainty closes it before release.
		f.terminal = len(calls) == 0
		for id := range f.acks {
			found := false
			for _, call := range calls {
				found = found || call == id
			}
			if !found {
				err = errors.New("unmatched acknowledgement")
			}
		}
		f.acks = map[string]bool{}
	}
	if err != nil {
		// Keep active held: an invalid early acknowledgement must not wake a
		// queued compact/continuation before stopped becomes permanent.
		f.mu.Unlock()
		f.stop("protocol_error", time.Now().UnixNano())
		f.mu.Lock()
	}
	f.active = false
	f.cancel = nil
	f.notify()
	f.mu.Unlock()
}

type journeyControlEvent struct {
	Kind     string `json:"kind"`
	Observed int64  `json:"observed_at_ns"`
	Call     string `json:"call_sha256,omitempty"`
	Status   string `json:"status,omitempty"`
	Reason   string `json:"reason,omitempty"`
	epoch    int
}

func (f *journeyFence) control(e journeyControlEvent) bool {
	if e.Observed <= 0 || e.Observed > time.Now().Add(time.Second).UnixNano() {
		f.stop("parse_error", time.Now().UnixNano())
		return false
	}
	if e.Kind == "stop" {
		f.stop(e.Reason, e.Observed)
		return false
	}
	if e.Kind == "end" {
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		for {
			f.mu.Lock()
			active, changed, stopped := f.active, f.changed, f.stopped
			f.mu.Unlock()
			if !active || stopped {
				break
			}
			select {
			case <-changed:
			case <-deadline.C:
				f.stop("transport_uncertain", e.Observed)
				return false
			}
		}
	}
	f.mu.Lock()
	valid := !f.stopped && e.epoch == f.observerEpoch
	if valid {
		switch e.Kind {
		case "begin":
			valid = !f.phase && !f.active && len(f.pending) == 0
			if valid {
				f.phase = true
				f.terminal = false
			}
		case "tool":
			_, hashErr := hex.DecodeString(e.Call)
			valid = len(e.Call) == 64 && hashErr == nil && e.Status == "completed" && f.phase
			if valid {
				if f.pending[e.Call] {
					delete(f.pending, e.Call)
					f.acknowledged[e.Call] = true
				} else if f.acknowledged[e.Call] {
					// Duplicate observations cannot confirm a new active response.
				} else if f.active && !f.seenCalls[e.Call] {
					f.acks[e.Call] = true
				} else {
					valid = false
				}
			}
		case "end":
			valid = f.phase && !f.active && len(f.pending) == 0 && f.terminal
			if valid {
				f.phase = false
				f.terminal = false
			}
		case "finish":
			valid = !f.phase && !f.active && len(f.pending) == 0
		default:
			valid = false
		}
	}
	if valid {
		f.journal = append(f.journal, map[string]any{"action": e.Kind, "at_ns": time.Now().UnixNano(), "observed_at_ns": e.Observed, "call_sha256": e.Call})
		f.notify()
	} else if !f.stopped {
		// Keep new acquire calls blocked while stop takes over this lock.
		f.phase = false
	}
	f.mu.Unlock()
	if !valid {
		f.stop("protocol_error", e.Observed)
	}
	return valid
}

// Private host-only socket: the model container/browser receives neither its
// path nor a mount containing it. One trusted observer owns a batch case.
func (f *journeyFence) bind(t *testing.T, cmd *exec.Cmd) func() {
	t.Helper()
	epoch, valid := f.nextObserver()
	if !valid {
		f.stop("protocol_error", time.Now().UnixNano())
	}
	dir, err := os.MkdirTemp("", "devin-observer-")
	require.NoError(t, err)
	path := filepath.Join(dir, "control.sock")
	listener, err := net.Listen("unix", path)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, 0600))
	cmd.Env = append(cmd.Env, "DEVIN_JOURNEY_CONTROL_SOCKET="+path)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReaderSize(conn, 4096)
		finished := false
		defer func() {
			if !finished {
				f.stop("control_lost", time.Now().UnixNano())
			}
		}()
		for {
			_ = conn.SetReadDeadline(time.Now().Add(180 * time.Second))
			line, err := reader.ReadSlice('\n')
			if err != nil {
				return
			}
			var event journeyControlEvent
			decoder := json.NewDecoder(bytes.NewReader(line))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&event) != nil || decoder.Decode(new(any)) != io.EOF {
				f.stop("parse_error", time.Now().UnixNano())
				return
			}
			event.epoch = epoch
			ok := f.control(event)
			_, err = fmt.Fprintf(conn, "{\"accepted\":%t}\n", ok)
			if err != nil {
				return
			}
			if event.Kind == "finish" && ok {
				finished = true
				return
			}
		}
	}()
	return func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			f.stop("control_lost", time.Now().UnixNano())
		}
		_ = os.RemoveAll(dir)
	}
}

func (f *journeyFence) nextObserver() (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	valid := !f.stopped && !f.phase && !f.active && len(f.pending) == 0
	if valid {
		f.observerEpoch++
		f.seenCalls = map[string]bool{}
		f.acknowledged = map[string]bool{}
		f.acks = map[string]bool{}
	} else if !f.stopped {
		f.phase = false
	}
	return f.observerEpoch, valid
}

func (f *journeyFence) snapshot() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var first map[string]any
	if f.first != nil {
		first = map[string]any{}
		for key, value := range f.first {
			first[key] = value
		}
	}
	return map[string]any{"installed": true, "phase_active": f.phase, "public_inflight": f.active, "stopped": f.stopped, "first_failure": first, "public_admitted": f.forwarded, "rejected_after_stop": f.denied,
		"pending_tools": len(f.pending), "owned_backend_death_verified": f.backendDead, "journal": append([]map[string]any(nil), f.journal...),
		"guarantee": "no public admission after gate closure; owned backend death stops local background sends; already remote inflight remains unknown"}
}

func runGuardedJourney(t *testing.T, cmd *exec.Cmd, app *application, admission *agentAdmission, fence *journeyFence) ([]byte, error) {
	t.Helper()
	admission.fence = fence
	release := fence.bind(t, cmd)
	if cancel := cmd.Cancel; cancel != nil {
		cmd.Cancel = func() error { fence.stop("deadline", time.Now().UnixNano()); return cancel() }
	}
	output, err := cmd.Output()
	if err != nil {
		fence.stop("client_exit", time.Now().UnixNano())
	}
	release()
	fence.mu.Lock()
	stopped := fence.stopped
	fence.mu.Unlock()
	if stopped && err == nil {
		err = errors.New("shared batch stopped")
	}
	return output, err
}
