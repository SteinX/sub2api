package devin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const UserStatusPath = "/exa.seat_management_pb.SeatManagementService/GetUserStatus"

// UsageStatus contains only the account's quota and plan data, without identity
// or credential fields. SeatManagement uses JSON Connect with Bearer auth.
// Wire reference: WncFht/devin2api e8547b6d60cb09fddc6debe44bafdfa69867a963,
// internal/ccpanel/upstream.go (GetUserStatus).
type UsageStatus struct {
	PlanName               string       `json:"plan_name,omitempty"`
	Daily                  *QuotaWindow `json:"-"`
	Weekly                 *QuotaWindow `json:"-"`
	AvailablePromptCredits *float64     `json:"available_prompt_credits,omitempty"`
	AvailableFlowCredits   *float64     `json:"available_flow_credits,omitempty"`
	AvailableFlexCredits   *float64     `json:"available_flex_credits,omitempty"`
	ACUConsumed            *float64     `json:"acu_consumed,omitempty"`
	ACULimit               *float64     `json:"acu_limit,omitempty"`
}

type QuotaWindow struct {
	RemainingPercent *float64
	ResetsAt         *time.Time
}

func (c *Client) Usage(ctx context.Context, token string) (*UsageStatus, error) {
	base := c.BaseURL
	if c.ValidateURL != nil {
		var err error
		base, err = c.ValidateURL(base)
		if err != nil {
			return nil, err
		}
	}
	token = NormalizeToken(token)
	body, err := json.Marshal(map[string]any{"metadata": struct {
		ClientInfo
		APIKey string `json:"api_key"`
	}{c.identity().TokenExchange, token}})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+UserStatusPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", "Bearer "+token)
	do := c.Do
	if do == nil {
		do = http.DefaultClient.Do
	}
	resp, err := do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > 4<<20 {
		return nil, errors.New("devin usage response too large")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(resp.StatusCode, raw)
	}
	return parseUsageStatus(raw)
}

func parseUsageStatus(raw []byte) (*UsageStatus, error) {
	var root map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		return nil, errors.New("invalid Devin usage response")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid Devin usage response")
	}
	root = usageKeys(root)
	user, ok := root["userstatus"].(map[string]any)
	if !ok {
		return nil, errors.New("devin returned no user status")
	}
	plan, _ := user["planstatus"].(map[string]any)
	info, _ := plan["planinfo"].(map[string]any)
	name, _ := info["planname"].(string)
	if name == "" {
		top, _ := root["planinfo"].(map[string]any)
		name, _ = top["planname"].(string)
	}
	return &UsageStatus{
		PlanName: name,
		Daily:    usageQuota(plan, "daily"), Weekly: usageQuota(plan, "weekly"),
		AvailablePromptCredits: usageNumber(plan["availablepromptcredits"]),
		AvailableFlowCredits:   usageNumber(plan["availableflowcredits"]),
		AvailableFlexCredits:   usageNumber(plan["availableflexcredits"]),
		ACUConsumed:            usageNumber(plan["acuconsumed"]),
		ACULimit:               usageNumber(plan["aculimit"]),
	}, nil
}

// Connect deployments return both proto snake_case and JSON camelCase keys.
func usageKeys(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	for key, value := range input {
		if child, ok := value.(map[string]any); ok {
			value = usageKeys(child)
		}
		result[strings.ToLower(strings.ReplaceAll(key, "_", ""))] = value
	}
	return result
}

func usageNumber(value any) *float64 {
	var raw string
	switch number := value.(type) {
	case json.Number:
		raw = string(number)
	case string:
		raw = number
	default:
		return nil
	}
	number, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return nil
	}
	return &number
}

func usageQuota(plan map[string]any, prefix string) *QuotaWindow {
	remaining := usageNumber(plan[prefix+"quotaremainingpercent"])
	if remaining != nil && (*remaining < 0 || *remaining > 100) {
		remaining = nil
	}
	var resetsAt *time.Time
	if reset := usageNumber(plan[prefix+"quotaresetatunix"]); reset != nil && *reset > 0 && *reset <= 253402300799 {
		t := time.Unix(int64(*reset), 0).UTC()
		resetsAt = &t
	}
	// ProtoJSON omits implicit-presence scalar zero values. A reported window
	// reset distinguishes an exhausted quota from an entirely absent window.
	if _, reported := plan[prefix+"quotaremainingpercent"]; !reported && resetsAt != nil {
		zero := 0.0
		remaining = &zero
	}
	if remaining == nil && resetsAt == nil {
		return nil
	}
	return &QuotaWindow{RemainingPercent: remaining, ResetsAt: resetsAt}
}
