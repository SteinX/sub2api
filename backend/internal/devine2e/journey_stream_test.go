//go:build devine2e

package devine2e

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const journeyStreamFinish = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

func streamPost(endpoint string) (*http.Response, error) {
	return (&http.Client{Timeout: 6 * time.Second}).Post(endpoint+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"`+fixtureModel+`","stream":true}`))
}

func TestJourneyStopStreamDeliversFirstChunkBeforeSourceCanFinish(t *testing.T) {
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"FIRST\"}}]}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		fmt.Fprint(w, journeyStreamFinish)
	}))
	defer backend.Close()
	f := newJourneyFence(nil, nil)
	require.True(t, f.control(controlEvent("begin")))
	p := newJourneyAdmission(t, &application{base: backend.URL}, fixtureModel, 3)
	p.fence = f
	response, err := streamPost(p.server.URL)
	require.NoError(t, err)
	defer response.Body.Close()
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, line, "FIRST")
	// The source is deliberately incapable of ending until this client witness.
	require.EqualValues(t, 1, calls.Load())
	require.Equal(t, true, f.snapshot()["public_inflight"])
	close(release)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return f.snapshot()["public_inflight"] == false }, time.Second, time.Millisecond)
	require.Equal(t, false, f.snapshot()["stopped"])
	require.Equal(t, false, f.snapshot()["buffered_transport"])
}

func TestJourneyStopStreamSDKCloseAtDoneStillRequiresOriginalEOF(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(fmt.Sprint(truncated), func(t *testing.T) {
			release := make(chan struct{})
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			var calls atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if n == 1 && truncated {
					w.Header().Set("Content-Length", strconv.Itoa(len(journeyStreamFinish)+7))
				}
				fmt.Fprint(w, journeyStreamFinish)
				w.(http.Flusher).Flush()
				if n == 1 {
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
			}))
			defer backend.Close()
			f := newJourneyFence(nil, nil)
			require.True(t, f.control(controlEvent("begin")))
			p := newJourneyAdmission(t, &application{base: backend.URL}, fixtureModel, 3)
			p.fence = f
			response, err := streamPost(p.server.URL)
			require.NoError(t, err)
			reader := bufio.NewReader(response.Body)
			for {
				line, err := reader.ReadString('\n')
				require.NoError(t, err)
				if strings.Contains(line, "[DONE]") {
					break
				}
			}
			require.NoError(t, response.Body.Close())
			continuation := make(chan int, 1)
			go func() {
				r, e := streamPost(p.server.URL)
				if e != nil {
					continuation <- 0
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				_ = r.Body.Close()
				continuation <- r.StatusCode
			}()
			require.Never(t, func() bool { return calls.Load() > 1 }, 100*time.Millisecond, 10*time.Millisecond, "no continuation before real source EOF")
			close(release)
			select {
			case status := <-continuation:
				if truncated {
					require.Equal(t, 402, status)
				} else {
					require.Equal(t, 200, status)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("original source proof did not resolve admission")
			}
			if truncated {
				require.EqualValues(t, 1, calls.Load())
				require.Equal(t, true, f.snapshot()["stopped"])
			} else {
				require.EqualValues(t, 2, calls.Load())
				require.Equal(t, false, f.snapshot()["stopped"])
			}
		})
	}
}

func TestJourneyStopStreamEarlyCancellationStopsInsteadOfDrainingPartialOutput(t *testing.T) {
	cancelled := make(chan struct{})
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"PARTIAL\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer backend.Close()
	f := newJourneyFence(nil, nil)
	require.True(t, f.control(controlEvent("begin")))
	p := newJourneyAdmission(t, &application{base: backend.URL}, fixtureModel, 3)
	p.fence = f
	response, err := streamPost(p.server.URL)
	require.NoError(t, err)
	_, err = bufio.NewReader(response.Body).ReadString('\n')
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("partial-output cancellation did not stop source")
	}
	require.Eventually(t, func() bool { return f.snapshot()["stopped"] == true }, time.Second, time.Millisecond)
	next, err := streamPost(p.server.URL)
	require.NoError(t, err)
	defer next.Body.Close()
	require.Equal(t, 402, next.StatusCode)
	require.EqualValues(t, 1, calls.Load())
}

func TestJourneyStopStreamDoneWithoutSourceCloseHasBoundedDrain(t *testing.T) {
	cancelled := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, journeyStreamFinish)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	}))
	defer backend.Close()
	f := newJourneyFence(nil, nil)
	require.True(t, f.control(controlEvent("begin")))
	p := newJourneyAdmission(t, &application{base: backend.URL}, fixtureModel, 3)
	p.fence = f
	response, err := streamPost(p.server.URL)
	require.NoError(t, err)
	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		if strings.Contains(line, "[DONE]") {
			break
		}
	}
	require.NoError(t, response.Body.Close())
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("same-source drain exceeded its bound")
	}
	require.Equal(t, true, f.snapshot()["stopped"])
}

func TestJourneyStopStreamNeverAcceptsLateSourceErrorOrOversizeAsEOF(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(fmt.Sprint(oversized), func(t *testing.T) {
			f := newJourneyFence(nil, nil)
			reader := io.MultiReader(strings.NewReader(journeyStreamFinish), brokenJourneyReader{})
			if oversized {
				reader = io.MultiReader(strings.NewReader(journeyStreamFinish), bytes.NewReader(make([]byte, (4<<20)+1)))
			}
			body := &journeyResponseBody{ReadCloser: io.NopCloser(reader), guard: f, stream: true}
			_, err := io.Copy(io.Discard, body)
			require.Error(t, err)
			_ = body.Close()
			require.False(t, body.eof)
			require.Equal(t, oversized, body.oversized)
			require.Equal(t, true, f.snapshot()["stopped"])
		})
	}
}

func TestJourneyStopStreamSourceRetainsOriginalDeadline(t *testing.T) {
	f := newJourneyFence(nil, nil)
	require.True(t, f.control(controlEvent("begin")))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	source, ownedCancel, err := f.acquire(ctx)
	require.NoError(t, err)
	defer ownedCancel()
	originalDeadline, _ := ctx.Deadline()
	sourceDeadline, _ := source.Deadline()
	require.Equal(t, originalDeadline, sourceDeadline)
	cancel()
	require.NoError(t, source.Err(), "downstream close alone cannot forge source EOF")
	f.stop("tool_error", time.Now().UnixNano())
	require.ErrorIs(t, source.Err(), context.Canceled)
}
