package devin

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/proto"
)

// RejectionKind names an explicit terminal refusal observed at the RPC boundary.
// It does not establish that a caller has not already received generation or usage.
type RejectionKind string

const (
	RejectionAuthentication RejectionKind = "authentication"
	RejectionRateLimit      RejectionKind = "rate_limit"
	RejectionModelCapacity  RejectionKind = "model_capacity"
)

// RetryableRejection requires the caller to additionally rule out execution
// evidence and admit another physical attempt through its durable budget.
func (e *Error) RetryableRejection() bool {
	if e == nil || e.Status >= 500 {
		return false
	}
	switch e.RejectionKind {
	case RejectionAuthentication, RejectionRateLimit, RejectionModelCapacity:
		return true
	default:
		return false
	}
}

type rpcFailure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details []struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"details"`
}

// classifyRejection discards arbitrary upstream text after extracting bounded
// protocol facts. HTTP 5xx bodies cannot override their transport status.
func classifyRejection(status int, failure rpcFailure, headers ...http.Header) *Error {
	e := &Error{Code: connectErrorCode(failure.Code), Status: status, TraceID: extractTraceID(failure.Message)}
	if status >= 500 {
		return e
	}
	switch {
	case status == http.StatusUnauthorized || status == 0 && e.Code == "unauthenticated":
		e.RejectionKind = RejectionAuthentication
	case status == http.StatusTooManyRequests || status == 0 && e.Code == "resource_exhausted":
		e.RejectionKind = RejectionRateLimit
	case status == 0 && e.Code == "unimplemented" && strings.Contains(strings.ToLower(failure.Message), "we are currently experiencing capacity issues with this serving model"):
		// This exact serving-model refusal was observed with Connect unimplemented;
		// generic unavailable, permission_denied and capacity wording are ambiguous.
		e.RejectionKind = RejectionModelCapacity
	}
	if !e.RetryableRejection() {
		return e
	}
	accept := func(delay time.Duration, ok bool) {
		if ok && (!e.HasRetryAfter || delay > e.RetryAfter) {
			e.RetryAfter, e.HasRetryAfter = delay, true
		}
	}
	now := time.Now()
	for _, header := range headers {
		for name, values := range header {
			if strings.EqualFold(name, "Retry-After") {
				for _, value := range values {
					accept(parseRetryAfter(value, now))
				}
			}
		}
	}
	for _, detail := range failure.Details {
		if detail.Type != "google.rpc.RetryInfo" {
			continue
		}
		data, err := base64.RawStdEncoding.DecodeString(detail.Value)
		if err != nil {
			data, err = base64.StdEncoding.DecodeString(detail.Value)
		}
		if err != nil {
			continue
		}
		var info errdetails.RetryInfo
		if proto.Unmarshal(data, &info) != nil || info.RetryDelay == nil || info.RetryDelay.CheckValid() != nil {
			continue
		}
		accept(info.RetryDelay.AsDuration(), info.RetryDelay.Seconds >= 0 && info.RetryDelay.Nanos >= 0)
	}
	return e
}

// parseRetryAfter never shortens a valid long cooldown. Saturating durations
// remain longer than a Fusion turn, allowing the caller to stop before resending.
func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	digits := true
	for _, c := range value {
		if c < '0' || c > '9' {
			digits = false
			break
		}
	}
	if digits {
		const maxDelay = time.Duration(1<<63 - 1)
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(maxDelay/time.Second) {
			return maxDelay, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	deadline, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(0, deadline.Sub(now)), true
}

func connectErrorCode(code string) string {
	switch code {
	case "canceled", "unknown", "invalid_argument", "deadline_exceeded", "not_found", "already_exists", "permission_denied", "resource_exhausted", "failed_precondition", "aborted", "out_of_range", "unimplemented", "internal", "unavailable", "data_loss", "unauthenticated":
		return code
	default:
		return "unknown"
	}
}
