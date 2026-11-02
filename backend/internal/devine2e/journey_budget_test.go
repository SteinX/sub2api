//go:build devine2e

package devine2e

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Only the authorized product runner installs this global, durable gate.
// Per-case admissions never create/reset it. Public-key quota reservations in
// the real gateway remain in force as a second, independent local limit.
type journeyBudget interface {
	Admit() (int, error)
	Complete(int, string) error
}

type authorizedJourneyBudget struct {
	db                        *sql.DB
	keyID                     int64
	mu                        sync.Mutex
	failed                    bool
	fatal                     atomic.Bool
	app                       *application
	key, model                string
	direct                    bool
	contextLimit, outputLimit int
}

func (b *authorizedJourneyBudget) directQuote() (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, b.app.base+"/v1/models?pricing_models="+url.QueryEscape(b.model)+"&context_tokens="+strconv.Itoa(b.contextLimit)+"&output_tokens="+strconv.Itoa(b.outputLimit), nil)
	if err != nil {
		return 0, errors.New("direct quote unavailable")
	}
	request.Header.Set("Authorization", "Bearer "+b.key)
	response, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(request)
	if err != nil {
		return 0, errors.New("direct quote unavailable")
	}
	defer response.Body.Close()
	var envelope struct {
		Data []struct {
			Model      string   `json:"model"`
			Identified bool     `json:"identified"`
			Calculable bool     `json:"calculable"`
			Cost       *float64 `json:"context_cost_upper_usd"`
		} `json:"data"`
	}
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&envelope) != nil || len(envelope.Data) != 1 {
		return 0, errors.New("direct quote unavailable")
	}
	price := envelope.Data[0]
	if price.Model != b.model || !price.Identified || !price.Calculable || price.Cost == nil || *price.Cost < 0 || math.IsInf(*price.Cost, 0) || math.IsNaN(*price.Cost) {
		return 0, errors.New("direct identified quote unavailable")
	}
	return *price.Cost, nil
}

func journeyPolicy(action string, arguments ...string) ([]byte, error) {
	script, err := filepath.Abs("../../../tools/devin-acceptance/authorized_journey.py")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "python3", append([]string{script, action, "--mode", os.Getenv("DEVIN_JOURNEY_RUN_MODE")}, arguments...)...)
	// stdout contains only structural decisions. No command argument contains a
	// credential; the runtime reference stays in the already approved parent.
	output, err := cmd.Output()
	if err != nil {
		return nil, errors.New("approved run policy denied or unavailable")
	}
	return output, nil
}

func (b *authorizedJourneyBudget) cost() (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var cost float64
	err := b.db.QueryRowContext(ctx, `SELECT greatest(k.quota_used,coalesce(sum(u.actual_cost),0),coalesce(sum(u.total_cost),0))
 FROM api_keys k LEFT JOIN usage_logs u ON u.api_key_id=k.id WHERE k.id=$1 GROUP BY k.id,k.quota_used`, b.keyID).Scan(&cost)
	return cost, err
}

func (b *authorizedJourneyBudget) Admit() (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.failed || b.fatal.Load() {
		return 0, errors.New("run stopped")
	}
	cost, err := b.cost()
	if err != nil {
		b.failed = true
		return 0, errors.New("local cost unavailable")
	}
	arguments := []string{"--cost", strconv.FormatFloat(cost, 'g', -1, 64)}
	if b.direct {
		quote, err := b.directQuote()
		if err != nil {
			b.failed = true
			return 0, err
		}
		arguments = append(arguments, "--estimated-cost", strconv.FormatFloat(quote, 'g', -1, 64))
	}
	if b.fatal.Load() {
		return 0, errors.New("run stopped")
	}
	output, err := journeyPolicy("admit", arguments...)
	var decision struct {
		Authorized bool `json:"authorized"`
		Receipt    int  `json:"receipt"`
	}
	if err != nil || json.Unmarshal(output, &decision) != nil || !decision.Authorized || decision.Receipt < 1 {
		b.failed = true
		return 0, errors.New("run budget denied")
	}
	return decision.Receipt, nil
}

func (b *authorizedJourneyBudget) Complete(receipt int, outcome string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	cost, err := b.cost()
	if err != nil {
		outcome = "uncertain"
		cost = -1
	}
	if outcome != "success" {
		b.failed = true
	}
	output, err := journeyPolicy("complete", "--receipt", fmt.Sprint(receipt), "--outcome", outcome, "--cost", strconv.FormatFloat(cost, 'g', -1, 64))
	var decision struct {
		Stopped    bool `json:"stopped"`
		Authorized bool `json:"authorized"`
	}
	if err == nil && (json.Unmarshal(output, &decision) != nil || !decision.Authorized || decision.Stopped) {
		err = errors.New("request accounting stopped the run")
	}
	if err != nil {
		b.failed = true
		return err
	}
	return nil
}

func (b *authorizedJourneyBudget) Stop(reason string, observed int64) error {
	b.fatal.Store(true)
	_, err := journeyPolicy("stop", "--reason", reason, "--observed-ns", strconv.FormatInt(observed, 10))
	return err
}

func (b *authorizedJourneyBudget) toolPolicy(action, operation string, calls []string) error {
	if b.fatal.Load() {
		return errors.New("run stopped")
	}
	arguments := []string{"--operation", operation}
	if calls != nil {
		encoded, err := json.Marshal(calls)
		if err != nil {
			return err
		}
		arguments = append(arguments, "--calls-json", string(encoded))
	}
	output, err := journeyPolicy(action, arguments...)
	var decision struct {
		Authorized bool `json:"authorized"`
		Stopped    bool `json:"stopped"`
	}
	if err != nil || json.Unmarshal(output, &decision) != nil || !decision.Authorized || decision.Stopped || b.fatal.Load() {
		return errors.New("durable tool admission denied or uncertain")
	}
	return nil
}

func (b *authorizedJourneyBudget) ToolBegin(operation string) error {
	return b.toolPolicy("tool_begin", operation, nil)
}
func (b *authorizedJourneyBudget) ToolReserve(operation string, calls []string) error {
	return b.toolPolicy("tool_reserve", operation, calls)
}

func (b *authorizedJourneyBudget) healthy() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.failed && !b.fatal.Load()
}

type journeyResponseBody struct {
	io.ReadCloser
	data        []byte
	eof         bool
	oversized   bool
	preloaded   bool
	mu          sync.Mutex
	guard       *journeyFence
	tools       *journeyToolBody
	stream      bool
	sourceError bool
	closeOnce   sync.Once
	closeErr    error
	drainTimer  *time.Timer
}

// A normal SDK may close its downstream stream immediately after [DONE]. The
// guarded observer must finish reading the owned gateway response first, rather
// than confusing that normal client behavior with an unknown upstream send.
// This buffers only the test admission boundary, preserves original bytes, and
// deliberately does not claim unbuffered first-token/stream timing coverage.
func preloadJourneyResponse(body io.ReadCloser) (*journeyResponseBody, error) {
	data, err := io.ReadAll(io.LimitReader(body, (4<<20)+1))
	_ = body.Close()
	capture := &journeyResponseBody{ReadCloser: io.NopCloser(bytes.NewReader(data)), data: data, eof: err == nil, preloaded: true}
	if len(data) > 4<<20 {
		capture.data = nil
		capture.eof = false
		capture.oversized = true
		return capture, errors.New("guarded response exceeds capture bound")
	}
	return capture, err
}

func (b *journeyResponseBody) Read(p []byte) (int, error) {
	if b.preloaded {
		return b.ReadCloser.Read(p)
	}
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	if len(b.data)+n <= 4<<20 {
		b.data = append(b.data, p[:n]...)
	} else {
		b.oversized = true
		b.data = nil
	}
	if err == io.EOF && !b.oversized {
		b.eof = true
	}
	if err != nil && err != io.EOF {
		b.sourceError = true
	}
	oversized := b.oversized
	if (b.eof || b.sourceError || oversized) && b.drainTimer != nil {
		b.drainTimer.Stop()
	}
	b.mu.Unlock()
	if oversized && b.guard != nil {
		return 0, errors.New("guarded response exceeds capture bound")
	}
	return n, err
}

func (b *journeyResponseBody) completedProtocol() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return !b.sourceError && !b.oversized && validJourneyCompletion(b.data, b.stream)
}

// This bounds draining the SAME source after an SDK closes at [DONE]. It never
// opens another connection, advances admission, or manufactures source EOF.
func (b *journeyResponseBody) boundDrain() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.eof && b.drainTimer == nil {
		b.drainTimer = time.AfterFunc(3*time.Second, func() { b.guard.stop("transport_uncertain", time.Now().UnixNano()); _ = b.ReadCloser.Close() })
	}
}

func (b *journeyResponseBody) downstreamClosed() {
	if !b.completedProtocol() {
		b.guard.stop("transport_uncertain", time.Now().UnixNano())
		return
	}
	b.boundDrain()
}

func (b *journeyResponseBody) Close() error {
	if b.guard == nil {
		return b.ReadCloser.Close()
	}
	b.closeOnce.Do(func() {
		if b.completedProtocol() {
			b.boundDrain()
			// ReverseProxy may stop reading after a downstream write error.
			// Finish observing the original body, independently of that copy.
			_, _ = io.Copy(io.Discard, b)
		}
		b.mu.Lock()
		valid := b.eof && !b.sourceError && !b.oversized && validJourneyCompletion(b.data, b.stream)
		if b.drainTimer != nil {
			b.drainTimer.Stop()
		}
		b.mu.Unlock()
		if !valid {
			b.guard.stop("transport_uncertain", time.Now().UnixNano())
		}
		b.closeErr = b.ReadCloser.Close()
	})
	return b.closeErr
}

// An HTTP 200 or a client process exit is insufficient: require a normal
// protocol terminal and, for SSE, the actual [DONE] boundary with no error.
func validJourneyCompletion(data []byte, stream bool) bool {
	normal := false
	consume := func(raw []byte) bool {
		if strictJourneyJSON(raw) != nil {
			return false
		}
		var v struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Finish *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(raw, &v) != nil || len(v.Error) > 0 && string(v.Error) != "null" {
			return false
		}
		for _, c := range v.Choices {
			if c.Finish != nil {
				if *c.Finish != "stop" && *c.Finish != "tool_calls" {
					return false
				}
				normal = true
			}
		}
		return true
	}
	if !stream {
		return consume(data) && normal
	}
	done := false
	reader := bufio.NewReader(bytes.NewReader(data))
	for {
		frame, err := readJourneyEvent(reader)
		if err == io.EOF {
			break
		}
		if err != nil {
			return false
		}
		raw, err := journeyEventData(frame)
		if err != nil {
			return false
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		if bytes.Equal(raw, []byte("[DONE]")) {
			done = true
			continue
		}
		if done || !consume(raw) {
			return false
		}
	}
	return normal && done
}
