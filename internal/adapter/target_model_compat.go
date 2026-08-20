package adapter

import (
	"encoding/json"
	"fmt"
	"strings"
)

const (
	dotsStudioNotePreviewFreeModel = "dots-studio/dots-3-note-preview:free"
	hy3MessagesMinOutputTokens     = 256
	dotsResponsesMinOutputTokens   = 512
)

type targetModelCompatStats struct {
	Model                      string
	Changed                    bool
	LowOutputRaised            bool
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

	switch model {
	case tencentHY3FreeModel:
		if path == "/messages" && raiseTargetOutputFloor(payload, "max_tokens", hy3MessagesMinOutputTokens) {
			stats.LowOutputRaised = true
		}
		if path == "/chat/completions" && normalizeHY3ForcedFunctionChoice(payload) {
			stats.ForcedToolChoiceNormalized = true
		}
	case dotsStudioNotePreviewFreeModel:
		if path == "/responses" && raiseTargetOutputFloor(payload, "max_output_tokens", dotsResponsesMinOutputTokens) {
			stats.LowOutputRaised = true
		}
	default:
		return body, stats, nil
	}

	stats.Changed = stats.LowOutputRaised || stats.ForcedToolChoiceNormalized
	if !stats.Changed {
		return body, stats, nil
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, stats, fmt.Errorf("encode target model compatibility request: %w", err)
	}
	return out, stats, nil
}

func raiseTargetOutputFloor(payload map[string]any, field string, minimum int) bool {
	value, ok := payload[field].(float64)
	if !ok || value <= 0 || value >= float64(minimum) {
		return false
	}
	payload[field] = float64(minimum)
	return true
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
