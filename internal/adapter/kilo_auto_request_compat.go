package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type kiloAutoRequestCompatStats struct {
	Matched                       bool
	Changed                       bool
	FlatToolsWrapped              int
	FlatToolFieldsMerged          int
	ToolSchemasRepaired           int
	OptionalToolsDropped          int
	ToolCallArgumentsNormalized   int
	LegacyFunctionRolesNormalized int
	MessageContentsNormalized     int
	ContentBlocksDropped          int
	MediaPlaceholdersInserted     int
	EmptyToolPlaceholdersRemoved  int
	FlatSchemaPromoted            bool
	UnrecoverableTools            int
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
	normalizeKiloAutoMessages(payload, &stats)
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
	mayDropInvalid := kiloAutoMayDropInvalidTools(payload["tool_choice"])
	normalized := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok || strings.ToLower(strings.TrimSpace(fmt.Sprint(tool["type"]))) != "function" {
			normalized = append(normalized, raw)
			continue
		}
		function, functionIsObject := tool["function"].(map[string]any)
		if functionIsObject {
			name, _ := function["name"].(string)
			if strings.TrimSpace(name) != "" {
				if mergeKiloAutoFlatToolFields(tool, function) {
					stats.FlatToolFieldsMerged++
					stats.Changed = true
				}
				if normalizeKiloAutoToolParameters(function) {
					stats.ToolSchemasRepaired++
					stats.Changed = true
				}
				normalized = append(normalized, raw)
				continue
			}
		}

		name, ok := tool["name"].(string)
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			stats.UnrecoverableTools++
			if mayDropInvalid {
				stats.OptionalToolsDropped++
				stats.Changed = true
				continue
			}
			normalized = append(normalized, raw)
			continue
		}
		if !functionIsObject {
			function = make(map[string]any)
		}
		function["name"] = name
		mergeKiloAutoFlatToolFields(tool, function)
		if normalizeKiloAutoToolParameters(function) {
			stats.ToolSchemasRepaired++
		}
		tool["function"] = function
		stats.FlatToolsWrapped++
		stats.Changed = true
		normalized = append(normalized, raw)
	}
	if len(normalized) == len(tools) {
		return
	}
	if len(normalized) == 0 {
		delete(payload, "tools")
		delete(payload, "parallel_tool_calls")
		if kiloAutoMayDropInvalidTools(payload["tool_choice"]) {
			delete(payload, "tool_choice")
		}
		return
	}
	payload["tools"] = normalized
}

func mergeKiloAutoFlatToolFields(tool, function map[string]any) bool {
	changed := false
	if raw, exists := tool["description"]; exists {
		if function["description"] == nil {
			if description, ok := raw.(string); ok && strings.TrimSpace(description) != "" {
				function["description"] = description
			}
		}
		delete(tool, "description")
		changed = true
	}
	if function["parameters"] == nil {
		var candidate any
		candidateFound := false
		if raw, exists := tool["parameters"]; exists && raw != nil {
			candidate = raw
			candidateFound = true
		} else if raw, exists := tool["input_schema"]; exists {
			candidate = raw
			candidateFound = true
		} else if raw, exists := tool["parameters"]; exists {
			candidate = raw
			candidateFound = true
		}
		if candidateFound {
			function["parameters"] = candidate
		}
	}
	for _, key := range []string{"parameters", "input_schema"} {
		if _, exists := tool[key]; exists {
			delete(tool, key)
			changed = true
		}
	}
	if raw, exists := tool["strict"]; exists {
		if function["strict"] == nil {
			if strict, ok := raw.(bool); ok {
				function["strict"] = strict
			}
		}
		delete(tool, "strict")
		changed = true
	}
	if _, exists := tool["name"]; exists {
		delete(tool, "name")
		changed = true
	}
	return changed
}

func kiloAutoMayDropInvalidTools(raw any) bool {
	if raw == nil {
		return true
	}
	choice, ok := raw.(string)
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "", "auto", "none":
		return true
	default:
		return false
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
	if values, ok := raw.([]any); ok && len(values) == 0 {
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
	if properties, exists := parameters["properties"]; exists {
		switch typed := properties.(type) {
		case nil:
			parameters["properties"] = map[string]any{}
			changed = true
		case []any:
			if len(typed) == 0 {
				parameters["properties"] = map[string]any{}
				changed = true
			}
		}
	}
	if required, exists := parameters["required"]; exists {
		switch typed := required.(type) {
		case nil:
			parameters["required"] = []any{}
			changed = true
		case string:
			name := strings.TrimSpace(typed)
			if name == "" {
				parameters["required"] = []any{}
			} else {
				parameters["required"] = []any{name}
			}
			changed = true
		}
	}
	return changed
}

func normalizeKiloAutoMessages(payload map[string]any, stats *kiloAutoRequestCompatStats) {
	messages, ok := payload["messages"].([]any)
	if !ok {
		return
	}
	for index, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if normalizeKiloAutoLegacyFunctionRole(messages, index, message) {
			stats.LegacyFunctionRolesNormalized++
			stats.Changed = true
		}
		if count := normalizeKiloAutoToolCallArguments(message); count > 0 {
			stats.ToolCallArgumentsNormalized += count
			stats.Changed = true
		}
		if changed, dropped, placeholder := normalizeKiloAutoMessageContent(message); changed {
			stats.MessageContentsNormalized++
			stats.ContentBlocksDropped += dropped
			stats.MediaPlaceholdersInserted += placeholder
			stats.Changed = true
		}
	}
}

func normalizeKiloAutoLegacyFunctionRole(messages []any, index int, message map[string]any) bool {
	if strings.ToLower(strings.TrimSpace(fmt.Sprint(message["role"]))) != "function" {
		return false
	}
	name, _ := message["name"].(string)
	name = strings.TrimSpace(name)
	if callID := matchingPreviousKiloAutoToolCallID(messages, index, name); callID != "" {
		message["role"] = "tool"
		message["tool_call_id"] = callID
		delete(message, "name")
		return true
	}
	label := "Function result"
	if name != "" {
		label += " (" + name + ")"
	}
	content := kiloAutoCompatContentString(message["content"])
	if strings.TrimSpace(content) != "" {
		message["content"] = label + ":\n" + content
	} else {
		message["content"] = label
	}
	message["role"] = "user"
	delete(message, "name")
	delete(message, "tool_call_id")
	return true
}

func matchingPreviousKiloAutoToolCallID(messages []any, index int, name string) string {
	if index <= 0 || name == "" {
		return ""
	}
	previous, ok := messages[index-1].(map[string]any)
	if !ok || strings.ToLower(strings.TrimSpace(fmt.Sprint(previous["role"]))) != "assistant" {
		return ""
	}
	calls, ok := previous["tool_calls"].([]any)
	if !ok {
		return ""
	}
	matchedID := ""
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		function, ok := call["function"].(map[string]any)
		if !ok || strings.TrimSpace(fmt.Sprint(function["name"])) != name {
			continue
		}
		id, _ := call["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" || matchedID != "" {
			return ""
		}
		matchedID = id
	}
	return matchedID
}

func normalizeKiloAutoToolCallArguments(message map[string]any) int {
	normalized := 0
	if calls, ok := message["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			function, ok := call["function"].(map[string]any)
			if ok && normalizeKiloAutoArguments(function) {
				normalized++
			}
		}
	}
	if function, ok := message["function_call"].(map[string]any); ok && normalizeKiloAutoArguments(function) {
		normalized++
	}
	return normalized
}

func normalizeKiloAutoArguments(function map[string]any) bool {
	raw, exists := function["arguments"]
	if !exists {
		return false
	}
	if raw == nil {
		function["arguments"] = "{}"
		return true
	}
	if text, ok := raw.(string); ok {
		if strings.TrimSpace(text) == "" {
			function["arguments"] = "{}"
			return true
		}
		return false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return false
	}
	function["arguments"] = string(encoded)
	return true
}

func normalizeKiloAutoMessageContent(message map[string]any) (bool, int, int) {
	content, exists := message["content"]
	if !exists || content == nil {
		return false, 0, 0
	}
	role := strings.ToLower(strings.TrimSpace(fmt.Sprint(message["role"])))
	if role == "tool" {
		if _, ok := content.(string); ok {
			return false, 0, 0
		}
		encoded, err := json.Marshal(content)
		if err != nil {
			return false, 0, 0
		}
		message["content"] = string(encoded)
		return true, 0, 0
	}
	switch typed := content.(type) {
	case string:
		return false, 0, 0
	case map[string]any:
		normalized, keep, _, dropped := normalizeKiloAutoContentBlock(typed)
		if !keep {
			if dropped {
				message["content"] = []any{kiloAutoTextBlock("[unsupported media omitted]")}
				return true, 1, 1
			}
			return false, 0, 0
		}
		message["content"] = []any{normalized}
		return true, 0, 0
	case []any:
		if len(typed) == 0 {
			return false, 0, 0
		}
		normalized := make([]any, 0, len(typed))
		changed := false
		dropped := 0
		for _, raw := range typed {
			block, keep, blockChanged, mediaDropped := normalizeKiloAutoContentBlock(raw)
			changed = changed || blockChanged
			if mediaDropped {
				dropped++
			}
			if keep {
				normalized = append(normalized, block)
			}
		}
		if len(normalized) == 0 {
			if dropped == 0 {
				return false, 0, 0
			}
			message["content"] = []any{kiloAutoTextBlock("[unsupported media omitted]")}
			return true, dropped, 1
		}
		if !changed && len(normalized) == len(typed) {
			return false, 0, 0
		}
		message["content"] = normalized
		return true, dropped, 0
	default:
		encoded, err := json.Marshal(content)
		if err != nil {
			return false, 0, 0
		}
		message["content"] = string(encoded)
		return true, 0, 0
	}
}

func normalizeKiloAutoContentBlock(raw any) (any, bool, bool, bool) {
	switch typed := raw.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil, false, true, false
		}
		return kiloAutoTextBlock(typed), true, true, false
	case map[string]any:
		typeName := strings.ToLower(strings.TrimSpace(fmt.Sprint(typed["type"])))
		switch typeName {
		case "text":
			_, ok := typed["text"].(string)
			if !ok {
				return kiloAutoTextBlock(kiloAutoCompatContentString(typed["text"])), true, true, false
			}
			if typed["type"] != "text" {
				typed["type"] = "text"
				return typed, true, true, false
			}
			return typed, true, false, false
		case "input_text":
			return kiloAutoTextBlock(kiloAutoCompatContentString(typed["text"])), true, true, false
		case "image_url":
			if imageURL, ok := kiloAutoImageURLValue(typed["image_url"]); ok {
				if _, isMap := typed["image_url"].(map[string]any); isMap && typed["type"] == "image_url" {
					return typed, true, false, false
				}
				return map[string]any{"type": "image_url", "image_url": imageURL}, true, true, false
			}
			return nil, false, true, true
		case "input_image":
			if imageURL, ok := kiloAutoImageURLValue(typed["image_url"]); ok {
				return map[string]any{"type": "image_url", "image_url": imageURL}, true, true, false
			}
			if imageURL, ok := kiloAutoImageURLValue(typed["url"]); ok {
				return map[string]any{"type": "image_url", "image_url": imageURL}, true, true, false
			}
			return nil, false, true, true
		case "image":
			if block, ok := kiloAutoAnthropicImageBlock(typed); ok {
				return block, true, true, false
			}
			return nil, false, true, true
		case "audio", "audio_url", "input_audio", "video", "video_url", "input_video":
			return nil, false, true, true
		case "file":
			return typed, true, false, false
		default:
			if text, ok := typed["text"].(string); ok && strings.TrimSpace(text) != "" {
				return kiloAutoTextBlock(text), true, true, false
			}
			encoded, err := json.Marshal(typed)
			if err != nil {
				return nil, false, false, false
			}
			return kiloAutoTextBlock(string(encoded)), true, true, false
		}
	default:
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil, false, false, false
		}
		return kiloAutoTextBlock(string(encoded)), true, true, false
	}
}

func kiloAutoTextBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

func kiloAutoImageURLValue(raw any) (map[string]any, bool) {
	switch typed := raw.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil, false
		}
		return map[string]any{"url": typed}, true
	case map[string]any:
		url, _ := typed["url"].(string)
		if strings.TrimSpace(url) == "" {
			return nil, false
		}
		return typed, true
	default:
		return nil, false
	}
}

func kiloAutoAnthropicImageBlock(block map[string]any) (map[string]any, bool) {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return nil, false
	}
	switch strings.ToLower(strings.TrimSpace(fmt.Sprint(source["type"]))) {
	case "base64":
		mediaType, _ := source["media_type"].(string)
		data, _ := source["data"].(string)
		if strings.TrimSpace(mediaType) == "" || strings.TrimSpace(data) == "" {
			return nil, false
		}
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:" + mediaType + ";base64," + data},
		}, true
	case "url":
		if imageURL, ok := kiloAutoImageURLValue(source["url"]); ok {
			return map[string]any{"type": "image_url", "image_url": imageURL}, true
		}
	}
	return nil, false
}

func kiloAutoCompatContentString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
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
	fmt.Printf("kilo_auto_request_compat path=/chat/completions flat_tools_wrapped=%d flat_tool_fields_merged=%d tool_schemas_repaired=%d optional_tools_dropped=%d tool_call_arguments_normalized=%d legacy_function_roles_normalized=%d message_contents_normalized=%d content_blocks_dropped=%d media_placeholders_inserted=%d empty_tool_placeholders_removed=%d flat_schema_promoted=%t unrecoverable_tools=%d changed=%t\n",
		stats.FlatToolsWrapped,
		stats.FlatToolFieldsMerged,
		stats.ToolSchemasRepaired,
		stats.OptionalToolsDropped,
		stats.ToolCallArgumentsNormalized,
		stats.LegacyFunctionRolesNormalized,
		stats.MessageContentsNormalized,
		stats.ContentBlocksDropped,
		stats.MediaPlaceholdersInserted,
		stats.EmptyToolPlaceholdersRemoved,
		stats.FlatSchemaPromoted,
		stats.UnrecoverableTools,
		stats.Changed,
	)
}
