package devin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUsageUsesSeatBearerProtocol(t *testing.T) {
	client := &Client{BaseURL: "https://server.codeium.com/", Do: func(req *http.Request) (*http.Response, error) {
		require.Equal(t, UserStatusPath, req.URL.Path)
		require.Equal(t, http.MethodPost, req.Method)
		require.Equal(t, "application/json", req.Header.Get("Content-Type"))
		require.Equal(t, "1", req.Header.Get("Connect-Protocol-Version"))
		require.Equal(t, "Bearer devin-session-token$test", req.Header.Get("Authorization"))
		var body struct{ Metadata map[string]any }
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
		require.Equal(t, "devin-session-token$test", body.Metadata["api_key"])
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{
			"userStatus":{"planStatus":{"dailyQuotaRemainingPercent":75,"weeklyQuotaRemainingPercent":"0",
			"dailyQuotaResetAtUnix":"1800000000","weeklyQuotaResetAtUnix":1800600000,
			"availablePromptCredits":0,"availableFlexCredits":"12.5","acuConsumed":"3.25","acuLimit":10,
			"planInfo":{"planName":"Teams"}}}}
		`))}, nil
	}}
	usage, err := client.Usage(context.Background(), "test")
	require.NoError(t, err)
	require.Equal(t, "Teams", usage.PlanName)
	require.Equal(t, 75.0, *usage.Daily.RemainingPercent)
	require.Equal(t, 0.0, *usage.Weekly.RemainingPercent)
	require.Equal(t, int64(1800000000), usage.Daily.ResetsAt.Unix())
	require.Equal(t, int64(1800600000), usage.Weekly.ResetsAt.Unix())
	require.Equal(t, 0.0, *usage.AvailablePromptCredits)
	require.Equal(t, 12.5, *usage.AvailableFlexCredits)
	require.Equal(t, 3.25, *usage.ACUConsumed)
	require.Nil(t, usage.AvailableFlowCredits)
}

func TestUsagePreservesMissingQuotaAndAcceptsSnakeCase(t *testing.T) {
	usage, err := parseUsageStatus([]byte(`{"user_status":{"plan_status":{
		"daily_quota_remaining_percent":null,"weekly_quota_remaining_percent":100,
		"daily_quota_reset_at_unix":"1800000000","weekly_quota_reset_at_unix":0}},
		"plan_info":{"plan_name":"Pro"}}`))
	require.NoError(t, err)
	require.Equal(t, "Pro", usage.PlanName)
	require.Nil(t, usage.Daily.RemainingPercent)
	require.NotNil(t, usage.Daily.ResetsAt)
	require.Equal(t, 100.0, *usage.Weekly.RemainingPercent)
	require.Nil(t, usage.Weekly.ResetsAt)

	usage, err = parseUsageStatus([]byte(`{"userStatus":{}}`))
	require.NoError(t, err)
	require.Nil(t, usage.Daily)
	require.Nil(t, usage.Weekly)

	usage, err = parseUsageStatus([]byte(`{"userStatus":{"planStatus":{"weeklyQuotaResetAtUnix":"1800600000"}}}`))
	require.NoError(t, err)
	require.Equal(t, 0.0, *usage.Weekly.RemainingPercent, "reset-backed omitted ProtoJSON zero is exhausted")
	require.Nil(t, usage.Daily)

	for _, value := range []string{`-1`, `101`, `"NaN"`, `"+Inf"`, `"bad"`, `true`, `null`} {
		usage, err = parseUsageStatus([]byte(`{"userStatus":{"planStatus":{"weeklyQuotaRemainingPercent":` + value + `}}}`))
		require.NoError(t, err)
		require.Nil(t, usage.Weekly, value)
	}
	for _, raw := range []string{`{}`, `{"userStatus":null}`, `null`, `invalid`, `{"userStatus":{}} {}`} {
		_, err = parseUsageStatus([]byte(raw))
		require.Error(t, err, raw)
	}
}

func TestUsageValidatesURLAndBoundsResponse(t *testing.T) {
	called := false
	client := &Client{BaseURL: "http://private.example", ValidateURL: func(string) (string, error) {
		return "", io.ErrUnexpectedEOF
	}, Do: func(*http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	}}
	_, err := client.Usage(context.Background(), "token")
	require.Error(t, err)
	require.False(t, called)

	client.ValidateURL = nil
	client.Do = func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat(" ", (4<<20)+1)))}, nil
	}
	_, err = client.Usage(context.Background(), "token")
	require.ErrorContains(t, err, "too large")
}
