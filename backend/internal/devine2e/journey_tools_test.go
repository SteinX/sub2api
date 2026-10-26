//go:build devine2e

package devine2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"time"
)

// Limits apply to a trusted CLI task/resume operation spanning HTTP requests.
// They are NOT model-provided session IDs, nor a fresh quota on every response.
type journeyToolLimits struct {
	Operations        []string `json:"operations"`
	PerOperation      int      `json:"per_operation"`
	Total             int      `json:"total"`
	NoToolsOperations []string `json:"no_tools_operations,omitempty"`
}

func (l journeyToolLimits) perOperation(operation string) int {
	for _, name := range l.NoToolsOperations {
		if name == operation {
			return 0
		}
	}
	return l.PerOperation
}

type journeyToolGate struct {
	limits          journeyToolLimits
	caseID, current string
	operations      map[string]int
	calls           map[string]bool
	completed       map[string]bool
	reserved        int
}

type journeyToolBudget interface {
	ToolBegin(string) error
	ToolReserve(string, []string) error
}

func (f *journeyFence) installToolLimits(limits journeyToolLimits) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tools != nil || f.active || f.phase || limits.PerOperation < 1 || limits.Total < 1 || len(limits.Operations) == 0 {
		return errors.New("invalid tool guard installation")
	}
	seen := map[string]bool{}
	for _, operation := range limits.Operations {
		if seen[operation] || !strings.HasSuffix(operation, "/task") && !strings.HasSuffix(operation, "/resume") {
			return errors.New("invalid fixed tool operation scope")
		}
		seen[operation] = true
	}
	for _, operation := range limits.NoToolsOperations {
		if !seen[operation] || !strings.HasSuffix(operation, "/resume") {
			return errors.New("invalid no-tool operation scope")
		}
	}
	f.tools = &journeyToolGate{limits: limits, operations: map[string]int{}, calls: map[string]bool{}, completed: map[string]bool{}}
	return nil
}

func (f *journeyFence) toolGuardEnabled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tools != nil
}

func (f *journeyFence) selectToolCase(caseID string) bool {
	f.toolMu.Lock()
	defer f.toolMu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped || f.active || f.phase || f.tools == nil {
		return false
	}
	for _, operation := range f.tools.limits.Operations {
		if operation == caseID+"/task" {
			f.tools.caseID = caseID
			return true
		}
	}
	return false
}

func (f *journeyFence) beginToolOperation(e journeyControlEvent) bool {
	f.toolMu.Lock()
	defer f.toolMu.Unlock()
	f.mu.Lock()
	g := f.tools
	operation := g.caseID + "/" + e.Operation
	approved := false
	for _, value := range g.limits.Operations {
		approved = approved || value == operation
	}
	_, claimed := g.operations[operation]
	valid := !f.stopped && e.epoch == f.observerEpoch && !f.phase && !f.active && len(f.pending) == 0 && approved && !claimed
	if valid {
		// A persistence failure is uncertain: never undo/retry this claim.
		g.current = operation
		g.operations[operation] = 0
	}
	f.mu.Unlock()
	if !valid {
		f.stop("protocol_error", e.Observed)
		return false
	}
	if b, ok := f.budget.(journeyToolBudget); ok {
		if err := b.ToolBegin(operation); err != nil {
			f.stop("tool_budget_unavailable", e.Observed)
			return false
		}
	} else if f.budget != nil {
		f.stop("tool_budget_unavailable", e.Observed)
		return false
	}
	f.mu.Lock()
	if f.stopped || e.epoch != f.observerEpoch || g.current != operation {
		f.mu.Unlock()
		f.stop("protocol_error", e.Observed)
		return false
	}
	f.phase, f.terminal = true, false
	f.journal = append(f.journal, map[string]any{"action": "tool_operation_begin", "operation": operation, "at_ns": time.Now().UnixNano()})
	f.notify()
	f.mu.Unlock()
	return true
}

// Atomic per SSE event / JSON response. Persistence happens without f.mu, so a
// first error closes admission and cancels the source even during slow disk IO.
func (f *journeyFence) reserveTools(calls []string) error {
	if len(calls) == 0 {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.stopped {
			return errors.New("batch stopped")
		}
		return nil
	}
	f.toolMu.Lock()
	defer f.toolMu.Unlock()
	f.mu.Lock()
	g := f.tools
	valid := g != nil && !f.stopped && f.phase && f.active && g.current != ""
	operation := ""
	if g != nil {
		operation = g.current
	}
	seen := map[string]bool{}
	for _, call := range calls {
		if len(call) != 64 || seen[call] || g == nil || g.calls[call] {
			valid = false
		}
		seen[call] = true
	}
	if !valid {
		f.mu.Unlock()
		f.stop("protocol_error", time.Now().UnixNano())
		return errors.New("invalid tool reservation")
	}
	if g.operations[operation]+len(calls) > g.limits.perOperation(operation) || g.reserved+len(calls) > g.limits.Total {
		f.journal = append(f.journal, map[string]any{"action": "tool_delivery_rejected", "operation": operation, "count": len(calls), "at_ns": time.Now().UnixNano()})
		f.mu.Unlock()
		f.stop("tool_budget_exhausted", time.Now().UnixNano())
		return errors.New("tool budget exhausted before delivery")
	}
	g.operations[operation] += len(calls)
	g.reserved += len(calls)
	for _, call := range calls {
		g.calls[call] = true
	}
	f.mu.Unlock()
	if b, ok := f.budget.(journeyToolBudget); ok {
		if err := b.ToolReserve(operation, calls); err != nil {
			f.stop("tool_budget_unavailable", time.Now().UnixNano())
			return errors.New("tool reservation persistence uncertain")
		}
	} else if f.budget != nil {
		f.stop("tool_budget_unavailable", time.Now().UnixNano())
		return errors.New("tool reservation persistence missing")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped || g.current != operation || !f.active || !f.phase {
		return errors.New("batch stopped during reservation")
	}
	f.journal = append(f.journal, map[string]any{"action": "tools_reserved_before_delivery", "operation": operation, "count": len(calls), "total": g.reserved, "at_ns": time.Now().UnixNano()})
	return nil
}

func (f *journeyFence) toolSnapshotLocked() map[string]any {
	if f.tools == nil {
		return map[string]any{"installed": false}
	}
	g := f.tools
	operations := map[string]int{}
	for name, count := range g.operations {
		operations[name] = count
	}
	return map[string]any{"installed": true, "per_operation": g.limits.PerOperation, "batch_max": g.limits.Total,
		"reserved": g.reserved, "completed": len(g.completed), "operations": operations, "scope": "fixed trusted task/resume slots, never public session IDs",
		"no_tools_operations": append([]string(nil), g.limits.NoToolsOperations...),
		"delivery":            "complete SSE event inspected before forwarding; JSON bounded before forwarding",
		"refunds":             false, "handoffs_direct": "not_applicable", "handoffs_fusion": "separate engine limit; not public tools"}
}

// Holds one bounded SSE event, NOT the complete streaming response. Original
// bytes (including CRLF, comments and data-line spacing) are delivered intact.
type journeyToolBody struct {
	io.ReadCloser
	reader            *bufio.Reader
	guard             *journeyFence
	stream            bool
	pending           []byte
	ids               map[int]string
	offered           map[string]bool
	initialized, done bool
}

func newJourneyToolBody(body io.ReadCloser, f *journeyFence, stream bool) *journeyToolBody {
	return &journeyToolBody{ReadCloser: body, reader: bufio.NewReaderSize(body, 4096), guard: f, stream: stream, ids: map[int]string{}, offered: map[string]bool{}}
}

func journeyEventData(frame []byte) ([]byte, error) {
	if bytes.HasPrefix(frame, []byte{0xef, 0xbb, 0xbf}) {
		return nil, errors.New("SSE BOM framing unsupported")
	}
	// This guard supports LF/CRLF. Never forward bare CR as opaque content:
	// standard SSE consumers treat it as a line boundary, too.
	for i, value := range frame {
		if value == '\r' && (i+1 == len(frame) || frame[i+1] != '\n') {
			return nil, errors.New("bare CR SSE framing unsupported")
		}
	}
	var data [][]byte
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimSuffix(line, []byte("\r"))
		if len(line) == 0 || line[0] == ':' {
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) && bytes.Contains(bytes.ToLower(line), []byte("error")) {
			return nil, errors.New("error SSE event")
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			value := bytes.TrimPrefix(line, []byte("data:"))
			value = bytes.TrimPrefix(value, []byte(" "))
			data = append(data, value)
		} else if !bytes.Equal(line, []byte("data")) && !bytes.HasPrefix(line, []byte("event:")) && !bytes.HasPrefix(line, []byte("id:")) && !bytes.HasPrefix(line, []byte("retry:")) {
			return nil, errors.New("unsupported SSE field")
		}
	}
	return bytes.Join(data, []byte("\n")), nil
}

// Go struct decoding accepts case-insensitive field names and duplicate keys,
// unlike a CLI looking up exact JSON keys. Reject that ambiguity BEFORE raw
// bytes can reach the CLI; EOF checks use the identical strict contract.
func strictJourneyJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	critical := map[string]bool{"choices": true, "index": true, "delta": true, "message": true,
		"tool_calls": true, "function_call": true, "id": true, "type": true, "function": true,
		"name": true, "arguments": true, "finish_reason": true, "error": true}
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("JSON nesting exceeds bound")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			keys := map[string]bool{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				for _, rune := range key {
					if rune > 127 {
						return errors.New("non-ASCII protocol key")
					}
				}
				folded := strings.ToLower(key)
				if !ok || keys[folded] || critical[folded] && key != folded {
					return errors.New("ambiguous JSON protocol key")
				}
				keys[folded] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("unexpected JSON delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("extra JSON data")
	}
	return nil
}

func readJourneyEvent(r *bufio.Reader) ([]byte, error) {
	var frame []byte
	continued := false
	for {
		line, err := r.ReadSlice('\n')
		frame = append(frame, line...)
		if len(frame) > 256<<10 {
			return nil, errors.New("SSE event exceeds bound")
		}
		if err == bufio.ErrBufferFull {
			continued = true
			continue
		}
		if err != nil {
			if err == io.EOF && len(frame) > 0 {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if !continued && (bytes.Equal(line, []byte("\n")) || bytes.Equal(line, []byte("\r\n"))) {
			return frame, nil
		}
		continued = false
	}
}

func (b *journeyToolBody) inspect(raw []byte) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return b.guard.reserveTools(nil)
	}
	if b.done {
		return errors.New("data after SSE terminal")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("[DONE]")) {
		b.done = true
		return b.guard.reserveTools(nil)
	}
	if err := strictJourneyJSON(raw); err != nil {
		return err
	}
	var v struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Index   int             `json:"index"`
			Delta   json.RawMessage `json:"delta"`
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &v) != nil || len(v.Choices) > 1 || len(v.Error) > 0 && string(v.Error) != "null" {
		return errors.New("unsupported tool envelope")
	}
	var calls []string
	if len(v.Choices) == 1 {
		c := v.Choices[0]
		if c.Index != 0 {
			return errors.New("multiple tool choices unsupported")
		}
		message := c.Message
		if b.stream {
			message = c.Delta
			if len(c.Message) > 0 && string(c.Message) != "null" {
				return errors.New("unexpected SSE message")
			}
		}
		var m struct {
			Function json.RawMessage `json:"function_call"`
			Tools    []struct {
				Index *int   `json:"index"`
				ID    string `json:"id"`
				Type  string `json:"type"`
			} `json:"tool_calls"`
		}
		if len(message) > 0 && string(message) != "null" && json.Unmarshal(message, &m) != nil {
			return errors.New("invalid tool message")
		}
		if len(m.Function) > 0 && string(m.Function) != "null" {
			return errors.New("legacy function call unsupported")
		}
		for ordinal, tool := range m.Tools {
			index := ordinal
			if b.stream {
				if tool.Index == nil {
					return errors.New("missing tool index")
				}
				index = *tool.Index
			}
			if index < 0 || index >= 64 || tool.Type != "" && tool.Type != "function" {
				return errors.New("unsupported tool type or index")
			}
			prior := b.ids[index]
			if prior == "" {
				if tool.ID == "" || len(tool.ID) > 1024 {
					return errors.New("tool arguments before identity")
				}
				id := callHash(tool.ID)
				if b.offered[id] {
					return errors.New("duplicate tool identity")
				}
				b.ids[index], b.offered[id] = tool.ID, true
				calls = append(calls, id)
			} else if tool.ID != "" && tool.ID != prior {
				return errors.New("changed tool identity")
			}
		}
	}
	return b.guard.reserveTools(calls)
}

func (b *journeyToolBody) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(b.pending) == 0 {
		var data, raw []byte
		var err error
		if b.stream {
			data, err = readJourneyEvent(b.reader)
			if err == nil {
				raw, err = journeyEventData(data)
			}
		} else if !b.initialized {
			b.initialized = true
			data, err = io.ReadAll(io.LimitReader(b.reader, (4<<20)+1))
			if len(data) > 4<<20 {
				err = errors.New("JSON response exceeds bound")
			}
			raw = data
		} else {
			return 0, io.EOF
		}
		if err != nil {
			if err != io.EOF {
				b.guard.stop("transport_uncertain", time.Now().UnixNano())
			}
			return 0, err
		}
		if err = b.inspect(raw); err != nil {
			b.guard.stop("protocol_error", time.Now().UnixNano())
			return 0, err // the entire rejecting event stays undelivered
		}
		b.pending = data
	}
	if err := b.guard.reserveTools(nil); err != nil {
		return 0, err
	}
	n := copy(p, b.pending)
	b.pending = b.pending[n:]
	return n, nil
}

func (b *journeyToolBody) offeredCalls() []string {
	result := make([]string, 0, len(b.offered))
	for id := range b.offered {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}
