//go:build devine2e

package devine2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func toolFixture(t *testing.T, kind string, per, total int, budget journeyBudget) *journeyFence {
	t.Helper()
	f := newJourneyFence(nil, budget)
	id := "opencode/" + kind + "/project"
	require.NoError(t, f.installToolLimits(journeyToolLimits{Operations: []string{id + "/task", id + "/resume"}, PerOperation: per, Total: total}))
	require.True(t, f.selectToolCase(id))
	e := controlEvent("begin")
	e.Operation = "task"
	require.True(t, f.control(e))
	return f
}

func toolStream(ids ...string) string {
	var b strings.Builder
	b.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[")
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"index":%d,"id":%q,"type":"function","function":{"name":"read","arguments":"{}"}}`, i, id)
	}
	b.WriteString("]}}]}\n\ndata: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	return b.String()
}

func deliverToolResponse(t *testing.T, f *journeyFence, wire string, stream bool) ([]byte, error) {
	t.Helper()
	_, cancel, err := f.acquire(context.Background())
	require.NoError(t, err)
	defer cancel()
	require.True(t, f.admit())
	tools := newJourneyToolBody(io.NopCloser(strings.NewReader(wire)), f, stream)
	capture := &journeyResponseBody{ReadCloser: tools, tools: tools, guard: f, stream: stream}
	delivered, readErr := io.ReadAll(capture)
	_ = capture.Close()
	f.finishResponse(&agentRequest{Status: 200, Stream: stream, response: capture})
	if readErr == nil {
		for _, call := range tools.offeredCalls() {
			e := controlEvent("tool")
			e.Call = call
			e.Status = "completed"
			require.True(t, f.control(e))
		}
	}
	return delivered, readErr
}

func TestJourneyToolLimitTwelfthDeliveredThirteenthWithheldForDirectAndFusion(t *testing.T) {
	for _, kind := range []string{"direct", "fusion"} {
		t.Run(kind, func(t *testing.T) {
			f := toolFixture(t, kind, 12, 48, nil)
			for i := 1; i <= 12; i++ {
				delivered, err := deliverToolResponse(t, f, toolStream(fmt.Sprintf("call-%d", i)), true)
				require.NoError(t, err)
				require.Contains(t, string(delivered), fmt.Sprintf("call-%d", i))
			}
			delivered, err := deliverToolResponse(t, f, toolStream("call-13"), true)
			require.Error(t, err)
			require.Empty(t, delivered, "no tool 13 identity/arguments reach an executor")
			state := f.snapshot()
			require.Equal(t, "tool_budget_exhausted", state["first_failure"].(map[string]any)["reason"])
			require.Equal(t, 12, state["tool_admission"].(map[string]any)["reserved"])
			_, _, err = f.acquire(context.Background())
			require.Error(t, err, "a continuation/compact cannot reopen")
		})
	}
}

func TestJourneyToolLimitRejectsEntireCrossingBatch(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			f := toolFixture(t, "direct", 12, 48, nil)
			for i := 0; i < 11; i++ {
				_, err := deliverToolResponse(t, f, toolStream(fmt.Sprintf("prior-%d", i)), true)
				require.NoError(t, err)
			}
			wire := toolStream("twelve", "thirteen")
			if !stream {
				wire = `{"choices":[{"message":{"tool_calls":[{"id":"twelve"},{"id":"thirteen"}]},"finish_reason":"tool_calls"}]}`
			}
			data, err := deliverToolResponse(t, f, wire, stream)
			require.Error(t, err)
			require.Empty(t, data)
			require.Equal(t, 11, f.snapshot()["tool_admission"].(map[string]any)["reserved"])
		})
	}
}

func TestJourneyToolLimitTwelveInOneEventAllowedAndACKCannotRefund(t *testing.T) {
	f := toolFixture(t, "fusion", 12, 48, nil)
	var ids []string
	for i := 0; i < 12; i++ {
		ids = append(ids, fmt.Sprintf("batch-%d", i))
	}
	wire := toolStream(ids...)
	data, err := deliverToolResponse(t, f, wire, true)
	require.NoError(t, err)
	require.Equal(t, wire, string(data))
	for _, id := range ids {
		e := controlEvent("tool")
		e.Call = callHash(id)
		e.Status = "completed"
		require.True(t, f.control(e), "duplicate observation cannot refund or increment completion")
	}
	snapshot := f.snapshot()["tool_admission"].(map[string]any)
	require.Equal(t, 12, snapshot["reserved"])
	require.Equal(t, 12, snapshot["completed"])
	data, err = deliverToolResponse(t, f, toolStream("thirteen"), true)
	require.Error(t, err)
	require.Empty(t, data)
}

func TestJourneyToolLimitCompletedErrorAcknowledgementNeverRefunds(t *testing.T) {
	f := toolFixture(t, "direct", 12, 12, nil)
	_, cancel, err := f.acquire(context.Background())
	require.NoError(t, err)
	defer cancel()
	require.True(t, f.admit())
	wire := toolStream("failed-tool")
	body := newJourneyToolBody(io.NopCloser(strings.NewReader(wire)), f, true)
	capture := &journeyResponseBody{ReadCloser: body, tools: body, guard: f, stream: true}
	_, err = io.ReadAll(capture)
	require.NoError(t, err)
	f.finishResponse(&agentRequest{Status: 200, Stream: true, response: capture})
	e := controlEvent("tool")
	e.Call, e.Status, e.Failed = callHash("failed-tool"), "error", true
	require.True(t, f.control(e))
	require.True(t, f.control(e), "duplicate failure feedback is not a new tool")
	for i := 1; i < 12; i++ {
		_, err := deliverToolResponse(t, f, toolStream(fmt.Sprint(i)), true)
		require.NoError(t, err)
	}
	snapshot := f.snapshot()["tool_admission"].(map[string]any)
	require.Equal(t, 12, snapshot["reserved"])
	require.Equal(t, 1, snapshot["failed"])
	require.Equal(t, 11, snapshot["completed"])
	require.Equal(t, 12, snapshot["terminal_acknowledged"])
	_, err = deliverToolResponse(t, f, toolStream("over-budget"), true)
	require.Error(t, err)
	require.Equal(t, "tool_budget_exhausted", f.snapshot()["first_failure"].(map[string]any)["reason"])
}

type oneByteReader struct{ io.Reader }

func (r oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return r.Reader.Read(p)
}

func TestJourneyToolLimitFragmentedMultilineSSEPreservesBytesAndCountsOnce(t *testing.T) {
	f := toolFixture(t, "direct", 12, 48, nil)
	_, cancel, err := f.acquire(context.Background())
	require.NoError(t, err)
	defer cancel()
	wire := ": comment\r\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[\r\ndata: {\"index\":0,\"id\":\"fragmented\",\"function\":{\"name\":\"read\"}}]}}]}\r\n\r\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	body := newJourneyToolBody(io.NopCloser(oneByteReader{strings.NewReader(wire)}), f, true)
	data, err := io.ReadAll(body)
	require.NoError(t, err)
	require.Equal(t, wire, string(data))
	require.True(t, validJourneyCompletion(data, true))
	calls, err := journeyResponseCalls(data, true)
	require.NoError(t, err)
	require.Equal(t, []string{callHash("fragmented")}, calls)
	require.Equal(t, 1, f.snapshot()["tool_admission"].(map[string]any)["reserved"])
	// A long data line whose final newline follows a Reader buffer boundary is
	// still one line, not a false blank event separator.
	long := strings.Repeat("x", 8192)
	r := newJourneyToolBody(io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\""+long+"\"}}]}\n\n")), f, true)
	data, err = io.ReadAll(r)
	require.NoError(t, err)
	require.Contains(t, string(data), long)
}

func TestJourneyToolLimitMalformedOrUnsupportedFrameNeverExposesArguments(t *testing.T) {
	for _, raw := range []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"id":"missing-index","function":{"arguments":"{}"}}]}}]}`,
		`{"choices":[{"delta":{"function_call":{"name":"read","arguments":"{}"}}}]}`,
		`{"choices":[{"index":0},{"index":1,"delta":{"tool_calls":[{"index":0,"id":"second"}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"same"},{"index":1,"id":"same"}]}}]}`,
	} {
		t.Run(fmt.Sprint(len(raw)), func(t *testing.T) {
			f := toolFixture(t, "direct", 12, 48, nil)
			data, err := deliverToolResponse(t, f, "data: "+raw+"\n\n", true)
			require.Error(t, err)
			require.Empty(t, data)
			require.Equal(t, 0, f.snapshot()["tool_admission"].(map[string]any)["reserved"])
		})
	}
}

func TestJourneyToolLimitRejectsDuplicateAndCaseShadowKeysBeforeDelivery(t *testing.T) {
	var tools []string
	for i := 0; i < 13; i++ {
		tools = append(tools, fmt.Sprintf(`{"index":%d,"id":"shadow-%d","function":{"name":"read","arguments":"{}"}}`, i, i))
	}
	legitimate := `{"choices":[{"delta":{"tool_calls":[` + strings.Join(tools, ",") + `]}}]}`
	for _, raw := range []string{
		strings.Replace(legitimate, `]}}]}`, `],"TOOL_CALLS":[]}}]}`, 1),
		strings.Replace(legitimate, `]}}]}`, `],"tool_calls":[]}}]}`, 1),
		strings.TrimSuffix(legitimate, "}") + `,"CHOICES":[]}`,
		strings.Replace(legitimate, `]}}]}`, `]}},"DELTA":{}}]}`, 1),
		`{"choices":[{"message":{"tool_calls":[{"id":"actual"}],"TOOL_CALLS":[]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":{"tool_calls":[{"id":"actual","ID":"shadow"}]},"finish_reason":"tool_calls"}]}`,
	} {
		for _, stream := range []bool{true, false} {
			t.Run(fmt.Sprintf("%t-%d", stream, len(raw)), func(t *testing.T) {
				f := toolFixture(t, "direct", 12, 48, nil)
				wire := raw
				if stream {
					wire = "data: " + raw + "\n\n"
				}
				data, err := deliverToolResponse(t, f, wire, stream)
				require.Error(t, err)
				require.Empty(t, data)
				require.False(t, validJourneyCompletion([]byte(wire), stream))
				require.Equal(t, 0, f.snapshot()["tool_admission"].(map[string]any)["reserved"])
			})
		}
	}
}

func TestJourneyToolLimitRejectsBareCRThatAClientWouldParseAsTools(t *testing.T) {
	f := toolFixture(t, "direct", 12, 48, nil)
	// An SSE consumer recognizes the CR-delimited data following the comment.
	// The guard must not forward it as an apparently empty LF-delimited event.
	wire := ": comment\r" + strings.ReplaceAll(toolStream("hidden-tool"), "\n\n", "\r\r") + "\n\n"
	data, err := deliverToolResponse(t, f, wire, true)
	require.Error(t, err)
	require.Empty(t, data)
	require.Equal(t, 0, f.snapshot()["tool_admission"].(map[string]any)["reserved"])
	require.False(t, validJourneyCompletion([]byte(wire), true))
}

func TestJourneyToolLimitRejectsBOMAndNoncanonicalSSEBeforeDelivery(t *testing.T) {
	var ids []string
	for i := 0; i < 13; i++ {
		ids = append(ids, fmt.Sprintf("hidden-%d", i))
	}
	for _, prefix := range []string{"\xef\xbb\xbf", " ", "\t"} {
		t.Run(fmt.Sprintf("%x", prefix), func(t *testing.T) {
			f := toolFixture(t, "direct", 12, 48, nil)
			wire := prefix + toolStream(ids...)
			data, err := deliverToolResponse(t, f, wire, true)
			require.Error(t, err)
			require.Empty(t, data)
			require.Equal(t, 0, f.snapshot()["tool_admission"].(map[string]any)["reserved"])
			require.False(t, validJourneyCompletion([]byte(wire), true))
		})
	}
}

func TestJourneyToolLimitSessionAndHTTPRequestCannotResetOperation(t *testing.T) {
	f := toolFixture(t, "direct", 12, 48, nil)
	for i := 0; i < 12; i++ {
		_, err := deliverToolResponse(t, f, toolStream(fmt.Sprint(i)), true)
		require.NoError(t, err)
	}
	_, err := deliverToolResponse(t, f, `{"choices":[{"finish_reason":"stop"}]}`, false)
	require.NoError(t, err)
	require.True(t, f.control(controlEvent("end")))
	// A new observer represents a new process/session, but the trusted task slot
	// has already been spent; neither the epoch nor session grants another 12.
	epoch, ok := f.nextObserver()
	require.True(t, ok)
	e := controlEvent("begin")
	e.epoch = epoch
	e.Operation = "task"
	require.False(t, f.control(e))
	require.Equal(t, 12, f.snapshot()["tool_admission"].(map[string]any)["reserved"])
}

type blockedToolBudget struct {
	entered, release chan struct{}
	once             sync.Once
	uncertain        bool
}

func (b *blockedToolBudget) Admit() (int, error)        { return 1, nil }
func (b *blockedToolBudget) Complete(int, string) error { return nil }
func (b *blockedToolBudget) ToolBegin(string) error     { return nil }
func (b *blockedToolBudget) ToolReserve(string, []string) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	if b.uncertain {
		return errors.New("unknown persistence")
	}
	return nil
}

func TestJourneyToolLimitStopWinsDuringReservationAndUncertainNeverRetries(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			budget := &blockedToolBudget{entered: make(chan struct{}), release: make(chan struct{}), uncertain: uncertain}
			f := toolFixture(t, "direct", 12, 48, budget)
			_, cancel, err := f.acquire(context.Background())
			require.NoError(t, err)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				body := newJourneyToolBody(io.NopCloser(strings.NewReader(toolStream("held"))), f, true)
				data, e := io.ReadAll(body)
				if len(data) != 0 {
					e = errors.New("exposed uncertain tool")
				}
				result <- e
			}()
			<-budget.entered
			if !uncertain {
				stopped := make(chan struct{})
				go func() { f.stop("permission_rejection", time.Now().UnixNano()); close(stopped) }()
				select {
				case <-stopped:
				case <-time.After(time.Second):
					t.Fatal("stop blocked on persistence")
				}
			}
			close(budget.release)
			require.Error(t, <-result)
			require.Equal(t, 1, f.snapshot()["tool_admission"].(map[string]any)["reserved"], "uncertain reservations stay spent")
			require.Equal(t, true, f.snapshot()["stopped"])
		})
	}
}

func TestJourneyToolLimitNoToolResumeWithholdsFirstCall(t *testing.T) {
	f := newJourneyFence(nil, nil)
	id := "opencode/fusion/long"
	require.NoError(t, f.installToolLimits(journeyToolLimits{Operations: []string{id + "/task", id + "/resume"}, PerOperation: 12, Total: 60, NoToolsOperations: []string{id + "/resume"}}))
	require.True(t, f.selectToolCase(id))
	e := controlEvent("begin")
	e.Operation = "task"
	require.True(t, f.control(e))
	_, err := deliverToolResponse(t, f, `{"choices":[{"finish_reason":"stop"}]}`, false)
	require.NoError(t, err)
	require.True(t, f.control(controlEvent("end")))
	e = controlEvent("begin")
	e.Operation = "resume"
	require.True(t, f.control(e))
	data, err := deliverToolResponse(t, f, toolStream("forbidden-memory-read"), true)
	require.Error(t, err)
	require.Empty(t, data)
	require.Equal(t, 0, f.snapshot()["tool_admission"].(map[string]any)["reserved"])
	require.Equal(t, "tool_budget_exhausted", f.snapshot()["first_failure"].(map[string]any)["reason"])
}
