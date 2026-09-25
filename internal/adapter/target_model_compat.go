package adapter

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

const (
	dotsStudioNotePreviewFreeModel        = "dots-studio/dots-3-note-preview:free"
	thinkingMachinesInklingSmallFreeModel = "thinkingmachines/inkling-small:free"
	thinkingMachinesInklingFreeModel      = "thinkingmachines/inkling:free"
	spaceBunnyAlphaModel                  = "stealth/space-bunny-alpha"
	targetModelMinOutputTokens            = 8192
	targetMessagesThinkingTokens          = 1024
	targetMinimalReasoningEffort          = "minimal"
)

type targetModelCompatStats struct {
	Model                      string
	Changed                    bool
	OutputBudgetNormalized     bool
	ReasoningNormalized        bool
	ForcedToolChoiceNormalized bool
}

func normalizeTargetFreeModelRequest(body []byte, upstreamPath string) ([]byte, targetModelCompatStats, error) {
	var stats targetModelCompatStats
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, stats, fmt.Errorf("invalid JSON body: %w", err)
	}

	model, _ := payload["model"].(string)
	model = strings.TrimPrefix(strings.TrimSpace(model), publicModelPrefix)
	stats.Model = model
	path := strings.TrimRight(strings.TrimSpace(upstreamPath), "/")

	reasoningEffort := targetMinimalReasoningEffort
	messagesThinking := map[string]any{
		"type":          "enabled",
		"budget_tokens": float64(targetMessagesThinkingTokens),
	}
	normalizeOutputBudget := true
	switch model {
	case tencentHY3FreeModel,
		dotsStudioNotePreviewFreeModel,
		thinkingMachinesInklingSmallFreeModel,
		thinkingMachinesInklingFreeModel,
		spaceBunnyAlphaModel:
	default:
		// Nemotron-family free models leak truncated reasoning into content
		// when thinking consumes the output budget; force reasoning off
		// without touching their output budget handling.
		if !strings.Contains(strings.ToLower(model), "nemotron") {
			return body, stats, nil
		}
		reasoningEffort = "none"
		messagesThinking = map[string]any{"type": "disabled"}
		normalizeOutputBudget = false
	}

	if normalizeTargetReasoning(payload, path, reasoningEffort, messagesThinking) {
		stats.ReasoningNormalized = true
	}
	if normalizeOutputBudget && normalizeTargetOutputBudget(payload, path, targetModelMinOutputTokens) {
		stats.OutputBudgetNormalized = true
	}
	if model == tencentHY3FreeModel && path == "/chat/completions" && normalizeHY3ForcedFunctionChoice(payload) {
		stats.ForcedToolChoiceNormalized = true
	}

	stats.Changed = stats.OutputBudgetNormalized || stats.ReasoningNormalized || stats.ForcedToolChoiceNormalized
	if !stats.Changed {
		return body, stats, nil
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, stats, fmt.Errorf("encode target model compatibility request: %w", err)
	}
	return out, stats, nil
}

func normalizeTargetReasoning(payload map[string]any, path string, reasoningEffort string, messagesThinking map[string]any) bool {
	remove := func(key string) bool {
		if _, exists := payload[key]; !exists {
			return false
		}
		delete(payload, key)
		return true
	}

	changed := false
	switch path {
	case "/chat/completions":
		changed = remove("reasoning") || changed
		changed = remove("thinking") || changed
		if effort, ok := payload["reasoning_effort"].(string); !ok || strings.ToLower(strings.TrimSpace(effort)) != reasoningEffort {
			payload["reasoning_effort"] = reasoningEffort
			changed = true
		}
	case "/messages":
		changed = remove("reasoning_effort") || changed
		changed = remove("reasoning") || changed
		if !reflect.DeepEqual(payload["thinking"], messagesThinking) {
			payload["thinking"] = messagesThinking
			changed = true
		}
	case "/responses":
		changed = remove("reasoning_effort") || changed
		changed = remove("thinking") || changed
		reasoning, ok := payload["reasoning"].(map[string]any)
		if !ok || reasoning["effort"] != reasoningEffort || len(reasoning) != 1 {
			payload["reasoning"] = map[string]any{"effort": reasoningEffort}
			changed = true
		}
	}
	return changed
}

func normalizeTargetOutputBudget(payload map[string]any, path string, minimum int) bool {
	canonical := ""
	switch path {
	case "/chat/completions", "/messages":
		canonical = "max_tokens"
	case "/responses":
		canonical = "max_output_tokens"
	default:
		return false
	}

	budget := float64(minimum)
	fields := []string{"max_tokens", "max_completion_tokens", "max_output_tokens"}
	for _, field := range fields {
		if value, ok := payload[field].(float64); ok && value > budget {
			budget = value
		}
	}

	changed := false
	for _, field := range fields {
		if field == canonical {
			continue
		}
		if _, exists := payload[field]; exists {
			delete(payload, field)
			changed = true
		}
	}
	if value, ok := payload[canonical].(float64); !ok || value != budget {
		payload[canonical] = budget
		changed = true
	}
	return changed
}

func normalizeHY3ForcedFunctionChoice(payload map[string]any) bool {
	choice, ok := payload["tool_choice"].(map[string]any)
	if !ok || strings.ToLower(strings.TrimSpace(fmt.Sprint(choice["type"]))) != "function" {
		return false
	}
	function, ok := choice["function"].(map[string]any)
	if !ok {
		return false
	}
	selectedName := strings.TrimSpace(fmt.Sprint(function["name"]))
	if selectedName == "" {
		return false
	}

	tools, ok := payload["tools"].([]any)
	if !ok {
		return false
	}
	var selected any
	matches := 0
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok || strings.ToLower(strings.TrimSpace(fmt.Sprint(tool["type"]))) != "function" {
			continue
		}
		toolFunction, ok := tool["function"].(map[string]any)
		if !ok || strings.TrimSpace(fmt.Sprint(toolFunction["name"])) != selectedName {
			continue
		}
		selected = raw
		matches++
	}
	if matches != 1 {
		return false
	}

	payload["tools"] = []any{selected}
	payload["tool_choice"] = "required"
	return true
}
