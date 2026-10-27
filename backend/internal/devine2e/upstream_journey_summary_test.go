//go:build devine2e

package devine2e

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/stretchr/testify/require"
)

func longFixtureReadName(path string) string {
	name := filepath.Base(path)
	if name == "price_math.py" || len(name) == 10 && strings.HasPrefix(name, "part0") && name[5] >= '0' && name[5] <= '7' && name[6:] == ".txt" {
		return name
	}
	return ""
}

func longFixtureHistoricalSummary(text string) (contextcompact.Summary, bool) {
	var summary contextcompact.Summary
	start := strings.Index(text, "\n{")
	ok := strings.HasPrefix(text, "Historical summary of completed work (") && start >= 0 &&
		json.Unmarshal([]byte(text[start+1:]), &summary) == nil && strings.Contains(summary.Goal, "BOUNDED_LONG_PROJECT")
	return summary, ok
}

// Restore only the scripted model's progress after the real gateway replaces a
// historical prefix. This is never used by the actual-read or receipt verifier.
func restoreLongFixtureReads(text string, reads map[string]int) {
	if summary, ok := longFixtureHistoricalSummary(text); ok {
		for _, name := range summary.Files {
			if name != "" && name == longFixtureReadName(name) {
				reads[name] = max(reads[name], 1)
			}
		}
	}
}

// The deterministic MODEL must answer the same existing summary contract as a
// real model. Derive facts and paired-read names only from its supplied history;
// never inspect project files, invent results or relax context/call budgets.
func boundedLongFixtureSummary(request *pb.GetChatMessageRequest) (string, bool) {
	if len(request.Tools) != 0 || request.Prompt != contextcompact.SummaryInstructionsV1 {
		return "", false
	}
	var items []apicompat.ResponsesInputItem
	for _, prompt := range request.ChatMessagePrompts {
		const prefix = "Summarize this completed history (JSON task data):\n"
		if strings.HasPrefix(prompt.Prompt, prefix) {
			if json.Unmarshal([]byte(strings.TrimPrefix(prompt.Prompt, prefix)), &items) != nil {
				return "", false
			}
			break
		}
	}
	goal, facts := "", ""
	files, calls := map[string]bool{}, map[string]string{}
	for _, item := range items {
		var content string
		if item.Type == "message" && json.Unmarshal(item.Content, &content) == nil {
			if summary, ok := longFixtureHistoricalSummary(content); ok {
				goal = summary.Goal
				facts += strings.Join(summary.Evidence, "\n") + "\n"
				for _, name := range summary.Files {
					if name != "" && name == longFixtureReadName(name) {
						files[name] = true
					}
				}
			} else if item.Role == "user" && strings.Contains(content, "BOUNDED_LONG_PROJECT") {
				goal = content
			}
		}
		if item.Type == "function_call" && item.Name == "read" {
			var args map[string]any
			if json.Unmarshal([]byte(item.Arguments), &args) == nil {
				for _, field := range []string{"filePath", "path", "file_path"} {
					if path, ok := args[field].(string); ok {
						calls[item.CallID] = longFixtureReadName(path)
					}
				}
			}
		}
		if name := calls[item.CallID]; item.Type == "function_call_output" && name != "" {
			files[name] = true
			if strings.HasPrefix(name, "part") {
				facts += item.Output + "\n"
			}
		}
		if item.Type == "function_call_output" {
			for _, fact := range longFactPattern.FindAllStringSubmatch(item.Output, -1) {
				files[fact[1]+".txt"] = true
				facts += fact[1] + "=" + fact[2] + "\n"
			}
		}
	}
	if goal == "" {
		return "", false
	}
	summary := contextcompact.Summary{Goal: goal, CurrentState: "Historical paired reads are listed in files; implementation, tests and final verification are not claimed complete.",
		Decisions: []string{}, Changes: []string{}, Evidence: []string{}, Checks: []string{}, OpenQuestions: []string{}, NextSteps: []string{"Continue the remaining project steps"}, Files: []string{}}
	var observed map[string]string
	_ = json.Unmarshal([]byte(longRecallText(facts, "")), &observed)
	for label, value := range observed {
		summary.Evidence = append(summary.Evidence, label+"="+value)
	}
	sort.Strings(summary.Evidence)
	for name := range files {
		summary.Files = append(summary.Files, name)
	}
	sort.Strings(summary.Files)
	encoded, err := json.Marshal(summary)
	return string(encoded), err == nil
}

func TestBoundedLongFixtureSummaryKeepsObservedFactsAndProgress(t *testing.T) {
	fact := strings.Repeat("a", 32)
	goal, _ := json.Marshal("BOUNDED_LONG_PROJECT: read all parts and repair the project; part07 was not read yet")
	items := []apicompat.ResponsesInputItem{
		{Type: "message", Role: "user", Content: goal},
		{Type: "function_call", Name: "read", CallID: "source", Arguments: `{"filePath":"/synthetic/price_math.py"}`},
		{Type: "function_call_output", CallID: "source", Output: "return subtotal + discount"},
		{Type: "function_call", Name: "read", CallID: "part", Arguments: `{"filePath":"/synthetic/context/part00.txt"}`},
		{Type: "function_call_output", CallID: "part", Output: "part00=" + fact},
		{Type: "function_call", Name: "bash", CallID: "bash-part", Arguments: `{"command":"cat context/part01.txt"}`},
		{Type: "function_call_output", CallID: "bash-part", Output: "part01=" + fact},
		{Type: "function_call", Name: "read", CallID: "pending", Arguments: `{"filePath":"/synthetic/context/part07.txt"}`},
	}
	request, err := devin.BuildRequest(contextcompact.BuildSummaryRequest(items, fixtureSidekick, 1024), "synthetic-summary")
	require.NoError(t, err)
	text, ok := boundedLongFixtureSummary(request)
	require.True(t, ok)
	parsed, err := contextcompact.ParseSummary(&apicompat.ResponsesResponse{Status: "completed", Output: []apicompat.ResponsesOutput{{Type: "message", Content: []apicompat.ResponsesContentPart{{Type: "output_text", Text: text}}}}})
	require.NoError(t, err)
	summary := *parsed
	require.Equal(t, []string{"part00.txt", "part01.txt", "price_math.py"}, summary.Files)
	require.Equal(t, []string{"part00=" + fact, "part01=" + fact}, summary.Evidence)
	historical := contextcompact.HistoricalMessage(summary, "synthetic-source-digest", "summary_fixture")
	var historicalText string
	require.NoError(t, json.Unmarshal(historical.Content, &historicalText))
	require.JSONEq(t, `{"part00":"`+fact+`","part01":"`+fact+`"}`, longRecallText(historicalText, ""))
	reads := map[string]int{}
	restoreLongFixtureReads(historicalText, reads)
	require.Equal(t, map[string]int{"price_math.py": 1, "part00.txt": 1, "part01.txt": 1}, reads)
	// The next actual model call continues at an unread file, rather than
	// replaying archived reads and consuming extra public tool budget.
	input, _ := json.Marshal([]apicompat.ResponsesInputItem{historical})
	next, err := devin.BuildRequest(&apicompat.ResponsesRequest{Model: fixtureSidekick, Input: input,
		Tools: []apicompat.ResponsesTool{{Type: "function", Name: "read", Parameters: json.RawMessage(`{"type":"object","properties":{"filePath":{"type":"string"}}}`)}}}, "synthetic-summary")
	require.NoError(t, err)
	j := projectJourney{workspace: "/synthetic", testScenario: "bounded_long_complete"}
	reply := &pb.GetChatMessageResponse{Usage: &pb.ModelUsageStats{}}
	j.reply(next, reply)
	require.Len(t, reply.DeltaToolCalls, 1)
	var args map[string]any
	require.NoError(t, json.Unmarshal([]byte(reply.DeltaToolCalls[0].ArgumentsJson), &args))
	require.Equal(t, "/synthetic/context/part02.txt", args["filePath"])
}
