//go:build devine2e

package devine2e

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/redis/go-redis/v9"
)

// Keep before/after item shapes and equality evidence, never conversation text.
func diagnoseAgentHistory(t *testing.T, app *application, keyID int64, records []agentRequest) {
	t.Helper()
	if len(records) == 0 {
		return
	}
	last := records[len(records)-1]
	if last.Session == "" {
		return
	}
	db := agentDatabase(t, app)
	var userID int64
	if db.QueryRow("SELECT user_id FROM api_keys WHERE id=$1", keyID).Scan(&userID) != nil {
		return
	}
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:" + os.Getenv("DEVIN_E2E_REDIS_PORT")})
	defer rdb.Close()
	stored, err := rdb.Get(context.Background(), "devin:fusion:session:"+fusion.Scope(userID, keyID)+":"+fusion.Digest(last.Session)).Bytes()
	if err != nil {
		return
	}
	var session fusion.Session
	if json.Unmarshal(stored, &session) != nil {
		return
	}
	var shapes []map[string]any
	for i := 0; i < len(session.Public) && i < len(last.items); i++ {
		a, b := session.Public[i], last.items[i]
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		var joined strings.Builder
		jsonBlocks := json.Unmarshal([]byte(b.Output), &parts) == nil && len(parts) > 0
		for _, part := range parts {
			if part.Type != "text" {
				jsonBlocks = false
			}
			joined.WriteString(part.Text)
		}
		shapes = append(shapes, map[string]any{"index": i, "stored_type": a.Type, "incoming_type": b.Type, "stored_role": a.Role, "incoming_role": b.Role,
			"equal": fusion.Digest(a) == fusion.Digest(b), "call_id_equal": a.CallID == b.CallID, "name_equal": a.Name == b.Name, "arguments_equal": a.Arguments == b.Arguments,
			"content_equal": string(a.Content) == string(b.Content), "stored_content_bytes": len(a.Content), "incoming_content_bytes": len(b.Content),
			"output_equal": a.Output == b.Output, "output_equal_after_trim": strings.TrimSpace(a.Output) == strings.TrimSpace(b.Output), "stored_output_bytes": len(a.Output), "incoming_output_bytes": len(b.Output), "replay_json_text_blocks": jsonBlocks, "replay_text_matches_stored": jsonBlocks && joined.String() == a.Output})
	}
	writeAgentArtifact(t, "agent-history-difference.json", map[string]any{"stored_count": len(session.Public), "incoming_count": len(last.items), "items": shapes,
		"state": session.State, "native_attempts": len(session.Attempts), "turn_requests": session.TurnRequests, "turn_cost_usd": session.TurnCostUSD})
}
