//go:build devine2e

package devine2e

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func controlEvent(kind string) journeyControlEvent {
	return journeyControlEvent{Kind: kind, Observed: time.Now().UnixNano()}
}

func TestJourneyStopBarrierRejectsContinuationCompactAndOtherCase(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"tool_calls":[{"id":"actual-public-call"}]},"finish_reason":"tool_calls"}]}`)
	}))
	defer backend.Close()
	f := newJourneyFence(nil, nil)
	require.True(t, f.control(controlEvent("begin")))
	a := newJourneyAdmission(t, &application{base: backend.URL}, fixtureModel, 20)
	a.fence = f
	b := newJourneyAdmission(t, &application{base: backend.URL}, fixtureSidekick, 20)
	b.fence = f
	post := func(endpoint, model string) int {
		r, err := http.Post(endpoint+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"`+model+`"}`))
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
		return r.StatusCode
	}
	require.Equal(t, 200, post(a.server.URL, fixtureModel))
	// Both queued model continuation and an auto-compact/other-case request
	// wait for actual CLI completion, not response bytes or process exit.
	blocked := make(chan int, 2)
	go func() { blocked <- post(a.server.URL, fixtureModel) }()
	go func() { blocked <- post(b.server.URL, fixtureSidekick) }()
	f.stop("permission_rejection", time.Now().UnixNano())
	require.Equal(t, 402, <-blocked)
	require.Equal(t, 402, <-blocked)
	require.EqualValues(t, 1, calls.Load())
	require.Equal(t, 1, f.snapshot()["public_admitted"])
	require.Equal(t, 402, post(a.server.URL, fixtureModel))
	late := controlEvent("tool")
	late.Call = callHash("actual-public-call")
	late.Status = "completed"
	require.False(t, f.control(late), "late success cannot reopen a stopped batch")
	require.False(t, f.control(controlEvent("begin")), "a next-case or restart cannot reopen")
}

func TestJourneyStopBarrierRequiresEveryActualToolAndNormalEnd(t *testing.T) {
	f := newJourneyFence(nil, nil)
	require.True(t, f.control(controlEvent("begin")))
	ctx, cancel, err := f.acquire(context.Background())
	require.NoError(t, err)
	defer cancel()
	require.NotNil(t, ctx)
	require.True(t, f.admit())
	// The trusted stdout reader may win the race with the response EOF.
	e := controlEvent("tool")
	e.Call = callHash("a")
	e.Status = "completed"
	require.True(t, f.control(e))
	record := &agentRequest{Status: 200, response: &journeyResponseBody{eof: true, data: []byte(`{"choices":[{"message":{"tool_calls":[{"id":"a"},{"id":"b"}]},"finish_reason":"tool_calls"}]}`)}}
	f.finishResponse(record)
	require.Equal(t, 1, f.snapshot()["pending_tools"])
	e.Call = callHash("b")
	e.Observed = time.Now().UnixNano()
	require.True(t, f.control(e))
	_, cancel2, err := f.acquire(context.Background())
	require.NoError(t, err)
	defer cancel2()
	f.finishResponse(&agentRequest{Status: 200, response: &journeyResponseBody{eof: true, data: []byte(`{"choices":[{"finish_reason":"stop"}]}`)}})
	require.True(t, f.control(controlEvent("end")))
	require.True(t, f.control(controlEvent("finish")))
	require.True(t, f.control(controlEvent("begin")), "explicit same-session resume is allowed after normal completion")
}

func TestJourneyStopResponseIdentityIsFailClosed(t *testing.T) {
	for _, data := range []string{
		`{"choices":[{"message":{"tool_calls":[{"id":""}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":{"tool_calls":[{"id":"a"},{"id":"a"}]},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a"}]}}]}` + "\n" + `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"b"}]},"finish_reason":"tool_calls"}]}` + "\ndata: [DONE]\n",
	} {
		_, err := journeyResponseCalls([]byte(data), strings.HasPrefix(data, "data:"))
		require.Error(t, err)
	}
	ids, err := journeyResponseCalls([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"a\"}]}}]}\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\ndata: [DONE]\n"), true)
	require.NoError(t, err)
	require.Equal(t, []string{callHash("a")}, ids)
}

func TestJourneyStopUnmatchedEarlyAckCannotReleaseWaitingRequest(t *testing.T) {
	f := newJourneyFence(nil, nil)
	require.True(t, f.control(controlEvent("begin")))
	_, cancel, err := f.acquire(context.Background())
	require.NoError(t, err)
	defer cancel()
	require.True(t, f.admit())
	e := controlEvent("tool")
	e.Call = callHash("not-in-public-response")
	e.Status = "completed"
	require.True(t, f.control(e))
	done := make(chan error, 1)
	go func() {
		_, cancel, err := f.acquire(context.Background())
		if cancel != nil {
			cancel()
		}
		done <- err
	}()
	f.finishResponse(&agentRequest{Status: 200, response: &journeyResponseBody{eof: true, data: []byte(`{"choices":[{"message":{"tool_calls":[{"id":"actual-public-call"}]},"finish_reason":"tool_calls"}]}`)}})
	require.Error(t, <-done)
	require.Equal(t, 1, f.snapshot()["public_admitted"])
	require.Equal(t, true, f.snapshot()["stopped"])
}

func TestJourneyStopCancelsExistingInflightAndDoesNotResend(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		close(entered)
		<-r.Context().Done()
		close(cancelled)
	}))
	defer backend.Close()
	f := newJourneyFence(nil, nil)
	require.True(t, f.control(controlEvent("begin")))
	p := newJourneyAdmission(t, &application{base: backend.URL}, fixtureModel, 20)
	p.fence = f
	finished := make(chan struct{})
	go func() {
		r, err := http.Post(p.server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"`+fixtureModel+`"}`))
		if err == nil {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
		close(finished)
	}()
	<-entered
	f.stop("reader_error", time.Now().UnixNano())
	<-cancelled
	<-finished
	require.Equal(t, 1, f.snapshot()["public_admitted"])
	require.Equal(t, "reader_error", f.snapshot()["first_failure"].(map[string]any)["reason"])
}

func TestJourneyStopNormalSummaryAllowsSamePhaseMainRequestWithoutDeadlock(t *testing.T) {
	f := newJourneyFence(nil, nil)
	require.True(t, f.control(controlEvent("begin")))
	_, cancel, err := f.acquire(context.Background())
	require.NoError(t, err)
	defer cancel()
	require.True(t, f.admit())
	f.finishResponse(&agentRequest{Status: 200, response: &journeyResponseBody{eof: true, data: []byte(`{"choices":[{"finish_reason":"stop"}]}`)}})
	// A real normal assistant response can precede automatic compaction or the
	// main prompt in the same operation. Phase end would deadlock that client.
	_, cancel2, err := f.acquire(context.Background())
	require.NoError(t, err)
	defer cancel2()
	require.True(t, f.admit())
	f.finishResponse(&agentRequest{Status: 200, response: &journeyResponseBody{eof: true, data: []byte(`{"choices":[{"finish_reason":"stop"}]}`)}})
	require.True(t, f.control(controlEvent("end")))
	require.Equal(t, 2, f.snapshot()["public_admitted"])
	stop := controlEvent("stop")
	stop.Reason = "reader_error"
	require.False(t, f.control(stop))
	require.False(t, f.control(controlEvent("begin")), "late compact restart cannot reopen after error")
}

func TestJourneyStopObserverExitKillsOwnedBackendAndCannotReopen(t *testing.T) {
	backend := exec.Command("python3", "-c", "import time;time.sleep(30)")
	require.NoError(t, backend.Start())
	done := make(chan struct{})
	go func() { _ = backend.Wait(); close(done) }()
	t.Cleanup(func() { _ = backend.Process.Kill(); <-done })
	f := newJourneyFence(&application{command: backend, done: done}, nil)
	observer := exec.Command("python3", "-c", `import socket,os,json,time
s=socket.socket(socket.AF_UNIX,socket.SOCK_STREAM);s.connect(os.environ['DEVIN_JOURNEY_CONTROL_SOCKET'])
s.sendall((json.dumps({'kind':'begin','observed_at_ns':time.time_ns()})+'\n').encode())
assert b'true' in s.recv(4096)
# Simulate an observer crash: no terminal/finish handshake.
s.close()
`)
	observer.Env = os.Environ()
	release := f.bind(t, observer)
	require.NoError(t, observer.Run())
	release()
	require.Equal(t, true, f.snapshot()["stopped"])
	require.Equal(t, true, f.snapshot()["owned_backend_death_verified"])
	select {
	case <-done:
	default:
		t.Fatal("owned backend was not reaped before reporting hard-stop success")
	}
	require.False(t, f.control(controlEvent("begin")))
}

func TestJourneyStopOldToolAckCannotConfirmNewResponse(t *testing.T) {
	f := newJourneyFence(nil, nil)
	require.True(t, f.control(controlEvent("begin")))
	response := func(id string) *agentRequest {
		return &agentRequest{Status: 200, response: &journeyResponseBody{eof: true, data: []byte(`{"choices":[{"message":{"tool_calls":[{"id":"` + id + `"}]},"finish_reason":"tool_calls"}]}`)}}
	}
	_, cancel, err := f.acquire(context.Background())
	require.NoError(t, err)
	defer cancel()
	f.finishResponse(response("old"))
	e := controlEvent("tool")
	e.Call = callHash("old")
	e.Status = "completed"
	require.True(t, f.control(e))
	_, cancel2, err := f.acquire(context.Background())
	require.NoError(t, err)
	defer cancel2()
	require.True(t, f.control(e), "a duplicate old terminal must never be assigned to the active response")
	f.finishResponse(response("new"))
	require.Equal(t, 1, f.snapshot()["pending_tools"])
	e.Call = callHash("new")
	require.True(t, f.control(e))
	_, cancel3, err := f.acquire(context.Background())
	require.NoError(t, err)
	defer cancel3()
	f.finishResponse(response("old"))
	require.Equal(t, true, f.snapshot()["stopped"], "provider UID reuse is ambiguous and must not consume an old ACK")
}

func TestJourneyStopDistinctCaseMayReusePublicToolIDButNotOldAck(t *testing.T) {
	f := newJourneyFence(nil, nil)
	for index := 1; index <= 2; index++ {
		epoch, ok := f.nextObserver()
		require.True(t, ok)
		event := func(kind string) journeyControlEvent { e := controlEvent(kind); e.epoch = epoch; return e }
		require.True(t, f.control(event("begin")))
		_, cancel, err := f.acquire(context.Background())
		require.NoError(t, err)
		defer cancel()
		f.finishResponse(&agentRequest{Status: 200, response: &journeyResponseBody{eof: true, data: []byte(`{"choices":[{"message":{"tool_calls":[{"id":"per-case-public-id"}]},"finish_reason":"tool_calls"}]}`)}})
		tool := event("tool")
		tool.Call = callHash("per-case-public-id")
		tool.Status = "completed"
		require.True(t, f.control(tool))
		_, cancel2, err := f.acquire(context.Background())
		require.NoError(t, err)
		defer cancel2()
		f.finishResponse(&agentRequest{Status: 200, response: &journeyResponseBody{eof: true, data: []byte(`{"choices":[{"finish_reason":"stop"}]}`)}})
		require.True(t, f.control(event("end")))
		require.True(t, f.control(event("finish")))
	}
	old := controlEvent("tool")
	old.epoch = 1
	old.Call = callHash("per-case-public-id")
	old.Status = "completed"
	require.False(t, f.control(old), "old observer cannot acknowledge a new case")
}

func TestJourneyStopExpiredDeadlineAndSnapshotDoNotShareMutableState(t *testing.T) {
	f := newJourneyFence(nil, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	f.stopOwned = func() bool { close(entered); <-release; return true }
	f.installDeadline(time.Now().Add(-time.Second))
	<-entered
	frozen := f.snapshot()["first_failure"].(map[string]any)
	close(release)
	<-f.stopDone
	_, changed := frozen["stop_actions_finished_at_ns"]
	require.False(t, changed, "a snapshot handed to JSON encoding must not share the stop goroutine's mutable map")
	require.Equal(t, true, f.snapshot()["owned_backend_death_verified"])
	require.Equal(t, "deadline", f.snapshot()["first_failure"].(map[string]any)["reason"])
}

func TestJourneyStopCapturesSourceBeforeClientClosesAtDone(t *testing.T) {
	wire := []byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"public-call\"}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	for _, truncated := range []bool{false, true} {
		reader := io.Reader(bytes.NewReader(wire))
		if truncated {
			reader = io.MultiReader(reader, brokenJourneyReader{})
		}
		capture, err := preloadJourneyResponse(io.NopCloser(reader))
		if truncated {
			require.Error(t, err)
			require.False(t, capture.eof)
			continue
		}
		require.NoError(t, err)
		// A client can close without asking for another transport EOF read.
		output := make([]byte, len(wire))
		n, err := capture.Read(output)
		require.NoError(t, err)
		require.Equal(t, len(wire), n)
		require.NoError(t, capture.Close())
		require.True(t, capture.eof)
		require.Equal(t, wire, capture.data)
		calls, err := journeyResponseCalls(capture.data, true)
		require.NoError(t, err)
		require.Equal(t, []string{callHash("public-call")}, calls)
	}
}

func TestJourneyStopPreloadDoesNotAcceptLimitOrCancelledSourceEOF(t *testing.T) {
	tooLarge, err := preloadJourneyResponse(io.NopCloser(bytes.NewReader(make([]byte, (4<<20)+1))))
	require.Error(t, err)
	require.True(t, tooLarge.oversized)
	require.False(t, tooLarge.eof)
	require.Nil(t, tooLarge.data)
	pipe, writer := io.Pipe()
	go func() { _, _ = writer.Write([]byte("data: [DONE]\n\n")); _ = writer.CloseWithError(context.Canceled) }()
	capture, err := preloadJourneyResponse(pipe)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, capture.eof)
}

type brokenJourneyReader struct{}

func (brokenJourneyReader) Read([]byte) (int, error) {
	return 0, fmt.Errorf("synthetic uncertain source close")
}
