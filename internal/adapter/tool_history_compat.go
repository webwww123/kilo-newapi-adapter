package adapter

import (
	"bytes"
	"encoding/json"
	"strings"
)

type toolHistoryCompatStats struct {
	EmptyArguments  int
	ObjectArguments int
}

// Kilo validates the string type even for completed historical tool calls.
// An empty string preserves absent argument information without inventing an
// input object. Pending calls and calls with ambiguous IDs remain untouched.
func normalizeKiloToolHistoryRequest(body []byte, path string) ([]byte, toolHistoryCompatStats, error) {
	var stats toolHistoryCompatStats
	if path != "/chat/completions" {
		return body, stats, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return body, stats, nil
	}
	var model string
	if json.Unmarshal(payload["model"], &model) != nil {
		return body, stats, nil
	}
	switch strings.TrimPrefix(model, publicModelPrefix) {
	case stepfunFlashFreeModel, "kilo-auto/free", thinkingMachinesInklingFreeModel, thinkingMachinesInklingSmallFreeModel:
	default:
		return body, stats, nil
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(payload["messages"], &messages) != nil {
		return body, stats, nil
	}
	callCounts := make(map[string]int)
	resultCounts := make(map[string]int)
	resultPositions := make(map[string]int)
	for i, message := range messages {
		switch toolHistoryString(message["role"]) {
		case "assistant":
			var calls []map[string]json.RawMessage
			if json.Unmarshal(message["tool_calls"], &calls) == nil {
				for _, call := range calls {
					callCounts[toolHistoryString(call["id"])]++
				}
			}
		case "tool":
			id := toolHistoryString(message["tool_call_id"])
			resultCounts[id]++
			resultPositions[id] = i
		}
	}
	for i, message := range messages {
		if toolHistoryString(message["role"]) != "assistant" {
			continue
		}
		var calls []map[string]json.RawMessage
		if json.Unmarshal(message["tool_calls"], &calls) != nil {
			continue
		}
		changed := false
		for _, call := range calls {
			id := toolHistoryString(call["id"])
			if id == "" || callCounts[id] != 1 || resultCounts[id] != 1 || resultPositions[id] <= i || toolHistoryString(call["type"]) != "function" {
				continue
			}
			var function map[string]json.RawMessage
			if json.Unmarshal(call["function"], &function) != nil || toolHistoryString(function["name"]) == "" {
				continue
			}
			arguments := bytes.TrimSpace(function["arguments"])
			switch {
			case len(arguments) == 0 || bytes.Equal(arguments, []byte("null")):
				function["arguments"] = json.RawMessage(`""`)
				stats.EmptyArguments++
			case arguments[0] == '{':
				encoded, err := json.Marshal(string(arguments))
				if err != nil {
					return body, toolHistoryCompatStats{}, err
				}
				function["arguments"] = encoded
				stats.ObjectArguments++
			default:
				continue
			}
			encoded, err := json.Marshal(function)
			if err != nil {
				return body, toolHistoryCompatStats{}, err
			}
			call["function"] = encoded
			changed = true
		}
		if changed {
			encoded, err := json.Marshal(calls)
			if err != nil {
				return body, toolHistoryCompatStats{}, err
			}
			message["tool_calls"] = encoded
		}
	}
	if stats.EmptyArguments+stats.ObjectArguments == 0 {
		return body, stats, nil
	}
	encoded, err := json.Marshal(messages)
	if err != nil {
		return body, toolHistoryCompatStats{}, err
	}
	payload["messages"] = encoded
	out, err := json.Marshal(payload)
	return out, stats, err
}

func toolHistoryString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}
