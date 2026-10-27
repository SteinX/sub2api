//go:build devine2e

package devine2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
)

func TestJourneyToolLimitLongWitnessRequiresEightDistinctFilesFromCompletedToolsInOneSuccessfulRequest(t *testing.T) {
	directory := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(directory, "workspace/context"), 0700))
	hashes := map[string]any{}
	parts := []any{}
	actualTools := []any{}
	items := []apicompat.ResponsesInputItem{}
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("context/part%02d.txt", i)
		prefix := fmt.Sprintf("part%02d=", i)
		raw := prefix + strings.Repeat(fmt.Sprint(i), 16384-len(prefix))
		require.Len(t, raw, 16384)
		require.NoError(t, os.WriteFile(filepath.Join(directory, "workspace", name), []byte(raw), 0600))
		id := fmt.Sprintf("actual-read-%d", i)
		hashes[name] = callHash(raw)
		parts = append(parts, map[string]any{"part": name, "call_sha256": callHash(id), "file_sha256": callHash(raw), "complete": true})
		actualTools = append(actualTools, map[string]any{"name": "read", "status": "completed", "call_sha256": callHash(id)})
		items = append(items, apicompat.ResponsesInputItem{Type: "function_call_output", CallID: id, Output: raw})
	}
	report := map[string]any{"long_file_sha256": hashes, "task": map[string]any{"long_reads": map[string]any{"parts": parts}, "tool_calls": actualTools}}
	healthy := func(values []apicompat.ResponsesInputItem) agentRequest {
		return agentRequest{Forwarded: true, Status: 200, items: values, response: &journeyResponseBody{eof: true, data: []byte(`{"choices":[{"finish_reason":"stop"}]}`)}}
	}
	require.Equal(t, true, longPublicContext(directory, []agentRequest{healthy(items)}, report)["complete"])
	t.Run("one_bash_output_with_all_files", func(t *testing.T) {
		encoded, _ := json.Marshal(report)
		var proof map[string]any
		require.NoError(t, json.Unmarshal(encoded, &proof))
		var text strings.Builder
		for _, item := range items {
			text.WriteString(item.Output + "\n")
		}
		task := proof["task"].(map[string]any)
		task["tool_calls"] = []any{map[string]any{"name": "bash", "status": "completed", "call_sha256": callHash("actual-bash")}}
		for _, value := range task["long_reads"].(map[string]any)["parts"].([]any) {
			value.(map[string]any)["call_sha256"] = callHash("actual-bash")
		}
		request := healthy([]apicompat.ResponsesInputItem{{Type: "function_call_output", CallID: "actual-bash", Output: text.String()}})
		require.Equal(t, true, longPublicContext(directory, []agentRequest{request}, proof)["complete"])
	})
	for _, kind := range []string{"split", "truncated", "unknown_id", "not_forwarded", "status", "source_eof", "incomplete_read", "duplicate_part"} {
		t.Run(kind, func(t *testing.T) {
			copied := append([]apicompat.ResponsesInputItem(nil), items...)
			records := []agentRequest{healthy(copied)}
			// Deep-copy private structural evidence; no native/model input is fabricated
			// by this unit fixture, which does not claim client journey coverage.
			encoded, _ := json.Marshal(report)
			var proof map[string]any
			require.NoError(t, json.Unmarshal(encoded, &proof))
			switch kind {
			case "split":
				records = []agentRequest{healthy(copied[:7]), healthy(copied[7:])}
			case "truncated":
				copied[7].Output = copied[7].Output[:100]
			case "unknown_id":
				copied[7].CallID = "unobserved-read"
			case "not_forwarded":
				records[0].Forwarded = false
			case "status":
				records[0].Status = 400
			case "source_eof":
				records[0].response.eof = false
			case "incomplete_read":
				proof["task"].(map[string]any)["long_reads"].(map[string]any)["parts"].([]any)[7].(map[string]any)["complete"] = false
			case "duplicate_part":
				p := proof["task"].(map[string]any)["long_reads"].(map[string]any)["parts"].([]any)
				p[7].(map[string]any)["part"] = p[0].(map[string]any)["part"]
				p[7].(map[string]any)["file_sha256"] = p[0].(map[string]any)["file_sha256"]
				copied[7].Output = copied[0].Output
			}
			require.Equal(t, false, longPublicContext(directory, records, proof)["complete"])
		})
	}
}
