package devin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
)

const capacityRejectionMessage = "We are currently experiencing capacity issues with this serving model. Please switch to a different model or try again later."

func TestDevinTerminalRejectionClassification(t *testing.T) {
	for _, tc := range []struct {
		name, code, message string
		status              int
		kind                RejectionKind
		projected           int
	}{
		{name: "HTTP authentication", status: 401, code: "unauthenticated", kind: RejectionAuthentication, projected: 401},
		{name: "HTTP account rate limit", status: 429, code: "resource_exhausted", kind: RejectionRateLimit, projected: 429},
		{name: "Connect authentication", code: "unauthenticated", kind: RejectionAuthentication, projected: 401},
		{name: "Connect account rate limit", code: "resource_exhausted", kind: RejectionRateLimit, projected: 429},
		{name: "known serving capacity", code: "unimplemented", message: capacityRejectionMessage, kind: RejectionModelCapacity, projected: 503},
		{name: "case insensitive known capacity", code: "unimplemented", message: strings.ToUpper(capacityRejectionMessage), kind: RejectionModelCapacity, projected: 503},
		{name: "generic capacity wording", code: "unimplemented", message: "capacity issues", projected: 502},
		{name: "permission remains permission", code: "permission_denied", message: capacityRejectionMessage, projected: 403},
		{name: "unavailable remains ambiguous", code: "unavailable", message: capacityRejectionMessage, projected: 502},
		{name: "deadline remains ambiguous", code: "deadline_exceeded", projected: 504},
		{name: "HTTP 5xx cannot spoof authentication", status: 503, code: "unauthenticated", projected: 503},
		{name: "HTTP 5xx cannot spoof rate limit", status: 502, code: "resource_exhausted", projected: 502},
		{name: "HTTP 5xx capacity remains ambiguous", status: 501, code: "unimplemented", message: capacityRejectionMessage, projected: 501},
		{name: "HTTP permission cannot spoof capacity", status: 403, code: "unimplemented", message: capacityRejectionMessage, projected: 403},
		{name: "HTTP mismatched authentication code", status: 400, code: "unauthenticated", projected: 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyRejection(tc.status, rpcFailure{Code: tc.code, Message: tc.message}, http.Header{"Retry-After": {"25"}})
			require.Equal(t, tc.kind, err.RejectionKind)
			require.Equal(t, tc.kind != "", err.RetryableRejection())
			require.Equal(t, tc.projected, err.HTTPStatus())
			require.Equal(t, tc.kind != "", err.HasRetryAfter)
			if tc.kind == "" {
				require.False(t, IsUnauthenticated(err), "ambiguous or conflicting failures must not trigger token recovery")
			}
		})
	}
}

func TestDevinHTTPRejectionBoundaryKeepsOnlyProtocolFacts(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		kind       RejectionKind
		code       string
	}{
		{"unauthorized HTML", "<html>private-token private-prompt</html>", 401, RejectionAuthentication, "unauthenticated"},
		{"rate limit malformed JSON", `{"broken":"private-token`, 429, RejectionRateLimit, "resource_exhausted"},
		{"arbitrary code discarded", `{"code":"private-token","message":"private-prompt"}`, 400, "", "unknown"},
		{"server failure body untrusted", `{"code":"unauthenticated","message":"private-token"}`, 503, "", "unauthenticated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sends := 0
			client := &Client{Do: func(*http.Request) (*http.Response, error) {
				sends++
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": {"120"}}, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			}}
			err := client.Chat(context.Background(), DefaultBaseURL, "test", &pb.GetChatMessageRequest{}, func(*pb.GetChatMessageResponse) error { return nil })
			var upstream *Error
			require.ErrorAs(t, err, &upstream)
			require.Equal(t, tc.kind, upstream.RejectionKind)
			require.Equal(t, tc.code, upstream.Code)
			require.Equal(t, "GetChatMessage", upstream.Operation)
			require.Equal(t, tc.kind != "", upstream.HasRetryAfter)
			if upstream.HasRetryAfter {
				require.Equal(t, 2*time.Minute, upstream.RetryAfter)
			}
			encoded, marshalErr := json.Marshal(upstream)
			require.NoError(t, marshalErr)
			require.NotContains(t, string(encoded), "private-token")
			require.NotContains(t, err.Error(), "private-prompt")
			require.Equal(t, 1, sends, "the protocol client does not retry physical inference itself")
		})
	}
}

func TestDevinConnectRetryHintsUseLongestTrustedDelay(t *testing.T) {
	info, err := proto.Marshal(&errdetails.RetryInfo{RetryDelay: durationpb.New(7*time.Second + 250*time.Millisecond)})
	require.NoError(t, err)
	for _, encoded := range []string{base64.StdEncoding.EncodeToString(info), base64.RawStdEncoding.EncodeToString(info)} {
		t.Run(encoded, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"error": map[string]any{
					"code": "resource_exhausted", "message": "private-token reset in 1 seconds",
					"details": []any{map[string]string{"type": "google.rpc.RetryInfo", "value": encoded}},
				},
				"metadata": map[string][]string{"retry-after": {"5"}},
			})
			require.NoError(t, err)
			client := &Client{Do: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Retry-After": {"3"}}, Body: io.NopCloser(strings.NewReader(string(frame(2, body))))}, nil
			}}
			err = client.Chat(context.Background(), DefaultBaseURL, "test", &pb.GetChatMessageRequest{}, func(*pb.GetChatMessageResponse) error { return nil })
			var upstream *Error
			require.ErrorAs(t, err, &upstream)
			require.Equal(t, RejectionRateLimit, upstream.RejectionKind)
			require.True(t, upstream.HasRetryAfter)
			require.Equal(t, 7*time.Second+250*time.Millisecond, upstream.RetryAfter)
			require.NotContains(t, err.Error(), "private-token")
		})
	}
}

func TestDevinRetryAfterDoesNotShortenLongCooldowns(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
		valid bool
	}{
		{"0", 0, true},
		{"1801", 1801 * time.Second, true},
		{"86400", 24 * time.Hour, true},
		{"184467440737095516160000", time.Duration(1<<63 - 1), true},
		{now.Add(2 * time.Hour).Format(http.TimeFormat), 2 * time.Hour, true},
		{now.Add(-time.Hour).Format(http.TimeFormat), 0, true},
		{"", 0, false},
		{"-1", 0, false},
		{"+1", 0, false},
		{"0.5", 0, false},
		{"1, 10", 0, false},
		{"reset in 4 seconds", 0, false},
		{"NaN", 0, false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			delay, ok := parseRetryAfter(tc.value, now)
			require.Equal(t, tc.valid, ok)
			require.Equal(t, tc.want, delay)
		})
	}
	upstream := classifyRejection(429, rpcFailure{Code: "resource_exhausted"}, http.Header{"retry-after": {"1", "3600", "invalid"}})
	require.True(t, upstream.HasRetryAfter)
	require.Equal(t, time.Hour, upstream.RetryAfter)
}

func TestDevinRetryInfoRejectsMalformedAndNegativeDetails(t *testing.T) {
	negative, err := proto.Marshal(&errdetails.RetryInfo{RetryDelay: durationpb.New(-time.Second)})
	require.NoError(t, err)
	empty, err := proto.Marshal(&errdetails.RetryInfo{})
	require.NoError(t, err)
	for _, detail := range []map[string]string{
		{"type": "google.rpc.RetryInfo", "value": "malformed-base64"},
		{"type": "google.rpc.RetryInfo", "value": base64.StdEncoding.EncodeToString([]byte("malformed protobuf"))},
		{"type": "google.rpc.RetryInfo", "value": base64.StdEncoding.EncodeToString(negative)},
		{"type": "google.rpc.RetryInfo", "value": base64.StdEncoding.EncodeToString(empty)},
		{"type": "another.RetryInfo", "value": base64.StdEncoding.EncodeToString(negative)},
	} {
		body, err := json.Marshal(map[string]any{"code": "resource_exhausted", "message": "reset in 1 seconds", "details": []any{detail}})
		require.NoError(t, err)
		var upstream *Error
		require.ErrorAs(t, responseError(429, body), &upstream)
		require.False(t, upstream.HasRetryAfter)
	}
}

func TestDevinAmbiguousTransportNeverBecomesRetryableRejection(t *testing.T) {
	for _, failure := range []error{io.ErrUnexpectedEOF, context.DeadlineExceeded, context.Canceled} {
		client := &Client{Do: func(*http.Request) (*http.Response, error) { return nil, failure }}
		err := client.Chat(context.Background(), DefaultBaseURL, "test", &pb.GetChatMessageRequest{}, func(*pb.GetChatMessageResponse) error { return nil })
		require.ErrorIs(t, err, failure)
		var upstream *Error
		require.False(t, errors.As(err, &upstream))
	}
	var missing *Error
	require.False(t, missing.RetryableRejection())
	require.False(t, (&Error{RejectionKind: RejectionModelCapacity, Status: 503}).RetryableRejection())
}
