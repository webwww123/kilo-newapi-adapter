package adapter

import (
	"bytes"
	"encoding/json"
	"strings"
)

type toolHistoryCompatStats struct {
	EmptyArguments          int
	ObjectArguments         int
	EmptyThinkingSignatures int
	EmptyThinkingTexts      int
}

// Kilo validates the string type even for completed historical tool calls.
// An empty string preserves absent argument information without inventing an
// input object. Pending calls and calls with ambiguous IDs remain untouched.
func normalizeKiloToolHistoryRequest(body []byte, path string) ([]byte, toolHistoryCompatStats, error) {
	var stats toolHistoryCompatStats
	if path != "/chat/completions" && path != "/messages" {
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
	case "nvidia/nemotron-3-super-120b-a12b:free":
		// Only the Messages history schema was verified for this model.
		if path != "/messages" {
			return body, stats, nil
		}
	case stepfunFlashFreeModel, "kilo-auto/free", thinkingMachinesInklingFreeModel, thinkingMachinesInklingSmallFreeModel,
		nexProFreeModel, nexMiniFreeModel, nemotronLightningFreeModel,
		lingFinFreeModel, lingSanteFreeModel, lingVLFreeModel:
	default:
		return body, stats, nil
	}
	var messages []map[string]json.RawMessage
	if json.Unmarshal(payload["messages"], &messages) != nil {
		return body, stats, nil
	}
	if path == "/messages" {
		return normalizeKiloHistoricalThinkingSignature(body, payload, messages, strings.TrimPrefix(model, publicModelPrefix))
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

// toolHistoryStringValue 只在原值确实是字符串时返回 true（含空字符串）。
// JSON null 反序列化到 string 不报错，必须单独判掉。
func toolHistoryStringValue(raw json.RawMessage) (string, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", false
	}
	var value string
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", false
	}
	return value, true
}

// requiresThinkingSignature 列出 Messages 协议下历史 thinking 必须带字符串 signature
// 的模型。缺 signature 时上游会整体拒绝请求（invalid_union: expected string,
// received undefined），补空字符串即通过，thinking 正文原样保留。
func requiresThinkingSignature(model string) bool {
	switch model {
	case thinkingMachinesInklingFreeModel, thinkingMachinesInklingSmallFreeModel,
		"nvidia/nemotron-3-super-120b-a12b:free",
		nexProFreeModel, nexMiniFreeModel, nemotronLightningFreeModel,
		lingFinFreeModel, lingSanteFreeModel, lingVLFreeModel:
		return true
	default:
		return false
	}
}

// Kilo's Messages schema requires string thinking and signature fields on every
// historical thinking block. Preserve existing string values, including empty
// ones, and replace only what is missing or of another type.
func normalizeKiloHistoricalThinkingSignature(body []byte, payload map[string]json.RawMessage, messages []map[string]json.RawMessage, model string) ([]byte, toolHistoryCompatStats, error) {
	var stats toolHistoryCompatStats
	if !requiresThinkingSignature(model) {
		return body, stats, nil
	}
	lastUser := -1
	for i, message := range messages {
		if toolHistoryString(message["role"]) == "user" {
			lastUser = i
		}
	}
	for i, message := range messages {
		if i >= lastUser || toolHistoryString(message["role"]) != "assistant" {
			continue
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(message["content"], &blocks) != nil {
			continue
		}
		changed := false
		for _, block := range blocks {
			if toolHistoryString(block["type"]) != "thinking" {
				continue
			}
			if _, ok := toolHistoryStringValue(block["signature"]); !ok {
				block["signature"] = json.RawMessage(`""`)
				stats.EmptyThinkingSignatures++
				changed = true
			}
			if _, ok := toolHistoryStringValue(block["thinking"]); !ok {
				block["thinking"] = json.RawMessage(`""`)
				stats.EmptyThinkingTexts++
				changed = true
			}
		}
		if changed {
			encoded, err := json.Marshal(blocks)
			if err != nil {
				return body, toolHistoryCompatStats{}, err
			}
			message["content"] = encoded
		}
	}
	if stats.EmptyThinkingSignatures+stats.EmptyThinkingTexts == 0 {
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
