//go:build devine2e

package devine2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
