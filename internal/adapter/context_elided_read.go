package adapter

import (
	"context"
	"encoding/json"
	"strings"
)

// A cache notice retained by either the original trim or recall identifies a
// dependency on an older Read. If original trim kept its exact call/result,
// recall must not remove that dependency even when it also drops the notice.
// This guard never inserts content or rewrites a tool result.
func contextRecallElidedReadGuard(ctx context.Context, items []any, baseline, selected []bool) string {
	if len(items) != len(baseline) || len(items) != len(selected) {
		return "candidate_invalid"
	}
	type readCall struct {
		index int
		key   string
	}
	type readResult struct {
		call   int
		result int
	}
	calls := make(map[string]readCall)
	previous := make(map[string]readResult)
	const notice = "Wasted call — file unchanged since your last Read. Refer to that earlier tool_result instead."
	for i, value := range items {
		if ctx.Err() != nil {
			return "deadline"
		}
		item, _ := value.(map[string]any)
		if contextItemRole(value) == "assistant" {
			tools, _ := item["tool_calls"].([]any)
			for _, raw := range tools {
				tool, _ := raw.(map[string]any)
				fn, _ := tool["function"].(map[string]any)
				if fn["name"] != "Read" {
					continue
				}
				id, _ := tool["id"].(string)
				args, _ := fn["arguments"].(string)
				if id == "" || len(args) > 8192 {
					continue
				}
				var parsed map[string]any
				if json.Unmarshal([]byte(args), &parsed) != nil {
					continue
				}
				path, _ := parsed["file_path"].(string)
				if path == "" {
					continue
				}
				key, _ := json.Marshal(parsed)
				calls[id] = readCall{index: i, key: string(key)}
			}
		}
		if contextItemRole(value) != "tool" {
			continue
		}
		id, _ := item["tool_call_id"].(string)
		call, ok := calls[id]
		content, text := item["content"].(string)
		if !ok || !text || call.index >= i {
			continue
		}
		if strings.TrimSpace(content) == notice {
			old, found := previous[call.key]
			if (baseline[i] || selected[i]) && found && baseline[old.call] && baseline[old.result] && (!selected[old.call] || !selected[old.result]) {
				return "retained_read_reference_lost"
			}
			continue
		}
		// Error or empty results do not establish a usable cached file body.
		if item["is_error"] == true || strings.TrimSpace(content) == "" {
			continue
		}
		previous[call.key] = readResult{call: call.index, result: i}
	}
	return ""
}
