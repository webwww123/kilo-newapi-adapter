package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type kiloAutoRequestCompatStats struct {
	Matched                      bool
	Changed                      bool
	FlatToolsWrapped             int
	ToolSchemasRepaired          int
	EmptyToolPlaceholdersRemoved int
	FlatSchemaPromoted           bool
	UnrecoverableTools           int
}

var kiloAutoJSONSchemaRootKeywords = map[string]struct{}{
	"$ref":                 {},
	"additionalProperties": {}, "allOf": {}, "anyOf": {}, "const": {},
	"contains": {}, "dependentRequired": {}, "dependentSchemas": {},
	"else": {}, "enum": {}, "exclusiveMaximum": {}, "exclusiveMinimum": {},
	"if": {}, "items": {}, "maxItems": {}, "maxLength": {},
	"maxProperties": {}, "maximum": {}, "minItems": {}, "minLength": {},
	"minProperties": {}, "minimum": {}, "multipleOf": {}, "not": {},
	"oneOf": {}, "pattern": {}, "patternProperties": {}, "prefixItems": {},
	"properties": {}, "propertyNames": {}, "required": {}, "then": {},
	"type": {}, "uniqueItems": {},
}

func normalizeKiloAutoChatRequest(body []byte) ([]byte, kiloAutoRequestCompatStats, error) {
	var stats kiloAutoRequestCompatStats
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return body, stats, nil
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return body, stats, nil
	}
	model, ok := payload["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		return body, stats, nil
	}
	stats = normalizeKiloAutoChatPayload(payload)
	if !stats.Changed {
		return body, stats, nil
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, stats, fmt.Errorf("encode Kilo Auto request compatibility: %w", err)
	}
	return out, stats, nil
}

func normalizeKiloAutoChatPayload(payload map[string]any) kiloAutoRequestCompatStats {
	var stats kiloAutoRequestCompatStats
	model, ok := payload["model"].(string)
	if !ok {
		return stats
	}
	model = strings.TrimSpace(model)
	if model != kiloAutoFreeModel && model != kiloAutoJSONPublicModel {
		return stats
	}
	stats.Matched = true
	normalizeKiloAutoTools(payload, &stats)
	removeKiloAutoTrailingToolPlaceholder(payload, &stats)
	if promoteFlatKiloAutoJSONSchema(payload) {
		stats.FlatSchemaPromoted = true
		stats.Changed = true
	}
	return stats
}

func normalizeKiloAutoTools(payload map[string]any, stats *kiloAutoRequestCompatStats) {
	tools, ok := payload["tools"].([]any)
	if !ok {
		return
	}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok || strings.ToLower(strings.TrimSpace(fmt.Sprint(tool["type"]))) != "function" {
			continue
		}
		function, functionIsObject := tool["function"].(map[string]any)
		if functionIsObject {
			name, _ := function["name"].(string)
			if strings.TrimSpace(name) != "" {
				if normalizeKiloAutoToolParameters(function) {
					stats.ToolSchemasRepaired++
					stats.Changed = true
				}
				continue
			}
		}

		name, ok := tool["name"].(string)
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			stats.UnrecoverableTools++
			continue
		}
		if !functionIsObject {
			function = make(map[string]any)
		}
		function["name"] = name
		if description, ok := tool["description"].(string); ok && strings.TrimSpace(description) != "" && function["description"] == nil {
			function["description"] = description
		}
		if parameters, ok := tool["parameters"].(map[string]any); ok && function["parameters"] == nil {
			function["parameters"] = parameters
		} else if inputSchema, ok := tool["input_schema"].(map[string]any); ok && function["parameters"] == nil {
			function["parameters"] = inputSchema
		}
		if strict, ok := tool["strict"].(bool); ok && function["strict"] == nil {
			function["strict"] = strict
		}
		if normalizeKiloAutoToolParameters(function) {
			stats.ToolSchemasRepaired++
		}
		tool["function"] = function
		for _, key := range []string{"name", "description", "parameters", "input_schema", "strict"} {
			delete(tool, key)
		}
		stats.FlatToolsWrapped++
		stats.Changed = true
	}
}

func normalizeKiloAutoToolParameters(function map[string]any) bool {
	raw, exists := function["parameters"]
	if !exists || raw == nil {
		function["parameters"] = map[string]any{
			"type":       "object",
			"properties": map[string]any{},
			"required":   []any{},
		}
		return true
	}
	parameters, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	changed := false
	if properties, exists := parameters["properties"]; exists && properties == nil {
		parameters["properties"] = map[string]any{}
		changed = true
	}
	if required, exists := parameters["required"]; exists && required == nil {
		parameters["required"] = []any{}
		changed = true
	}
	return changed
}

func removeKiloAutoTrailingToolPlaceholder(payload map[string]any, stats *kiloAutoRequestCompatStats) {
	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) < 2 {
		return
	}
	start := len(messages)
	for start > 0 && removableKiloAutoEmptyUserMessage(messages[start-1]) {
		start--
	}
	if start == len(messages) || start == 0 {
		return
	}
	if !kiloAutoHasCompletedAdjacentToolTurn(messages[:start]) {
		return
	}
	payload["messages"] = append([]any(nil), messages[:start]...)
	stats.EmptyToolPlaceholdersRemoved += len(messages) - start
	stats.Changed = true
}

func removableKiloAutoEmptyUserMessage(raw any) bool {
	message, ok := raw.(map[string]any)
	if !ok || strings.ToLower(strings.TrimSpace(fmt.Sprint(message["role"]))) != "user" {
		return false
	}
	for key := range message {
		if key != "role" && key != "content" {
			return false
		}
	}
	return lingUserContentIsEmpty(message["content"])
}

func kiloAutoHasCompletedAdjacentToolTurn(messages []any) bool {
	toolIDs := make(map[string]struct{})
	index := len(messages) - 1
	for index >= 0 {
		message, ok := messages[index].(map[string]any)
		if !ok || strings.ToLower(strings.TrimSpace(fmt.Sprint(message["role"]))) != "tool" {
			break
		}
		toolCallID, _ := message["tool_call_id"].(string)
		toolCallID = strings.TrimSpace(toolCallID)
		if toolCallID == "" {
			return false
		}
		toolIDs[toolCallID] = struct{}{}
		index--
	}
	if len(toolIDs) == 0 || index < 0 {
		return false
	}
	assistant, ok := messages[index].(map[string]any)
	if !ok || strings.ToLower(strings.TrimSpace(fmt.Sprint(assistant["role"]))) != "assistant" {
		return false
	}
	calls, ok := assistant["tool_calls"].([]any)
	if !ok {
		return false
	}
	matched := make(map[string]struct{}, len(calls))
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := call["id"].(string)
		id = strings.TrimSpace(id)
		if id != "" {
			matched[id] = struct{}{}
		}
	}
	for id := range toolIDs {
		if _, ok := matched[id]; !ok {
			return false
		}
	}
	if len(matched) != len(toolIDs) {
		return false
	}
	return true
}

func promoteFlatKiloAutoJSONSchema(payload map[string]any) bool {
	responseFormat, ok := payload["response_format"].(map[string]any)
	if !ok || strings.ToLower(strings.TrimSpace(fmt.Sprint(responseFormat["type"]))) != lingJSONSchemaFormat {
		return false
	}
	container, ok := responseFormat["json_schema"].(map[string]any)
	if !ok {
		return false
	}
	if _, exists := container["schema"]; exists {
		return false
	}
	schema := make(map[string]any)
	hasSchemaKeyword := false
	for key, value := range container {
		switch key {
		case "name", "description", "strict":
			continue
		}
		schema[key] = value
		if _, ok := kiloAutoJSONSchemaRootKeywords[key]; ok {
			hasSchemaKeyword = true
		}
	}
	if !hasSchemaKeyword {
		return false
	}
	for key := range schema {
		delete(container, key)
	}
	container["schema"] = schema
	return true
}

func logKiloAutoRequestCompat(stats kiloAutoRequestCompatStats) {
	if !stats.Matched || (!stats.Changed && stats.UnrecoverableTools == 0) {
		return
	}
	fmt.Printf("kilo_auto_request_compat path=/chat/completions flat_tools_wrapped=%d tool_schemas_repaired=%d empty_tool_placeholders_removed=%d flat_schema_promoted=%t unrecoverable_tools=%d changed=%t\n",
		stats.FlatToolsWrapped,
		stats.ToolSchemasRepaired,
		stats.EmptyToolPlaceholdersRemoved,
		stats.FlatSchemaPromoted,
		stats.UnrecoverableTools,
		stats.Changed,
	)
}
