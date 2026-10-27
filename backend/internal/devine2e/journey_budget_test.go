//go:build devine2e

package devine2e

import (
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
	if b.failed {
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

func (b *authorizedJourneyBudget) healthy() bool { b.mu.Lock(); defer b.mu.Unlock(); return !b.failed }

type journeyResponseBody struct {
	io.ReadCloser
	data      []byte
	eof       bool
	oversized bool
}

func (b *journeyResponseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if len(b.data)+n <= 4<<20 {
		b.data = append(b.data, p[:n]...)
	} else {
		b.oversized = true
		b.data = nil
	}
	if err == io.EOF && !b.oversized {
		b.eof = true
	}
	return n, err
}

// An HTTP 200 or a client process exit is insufficient: require a normal
// protocol terminal and, for SSE, the actual [DONE] boundary with no error.
func validJourneyCompletion(data []byte, stream bool) bool {
	normal := false
	consume := func(raw []byte) bool {
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
	for _, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("event:")) && bytes.Contains(line, []byte("error")) {
			return false
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		raw := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
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
