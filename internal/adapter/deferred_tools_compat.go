package adapter

import "strings"

// Space Bunny 拒绝延迟工具标志。仅在所有自定义工具定义已经齐全、没有
// 搜索或引用历史时改为直接注册；不删除工具、不改名称、参数或调用历史。
func normalizeSpaceBunnyDeferredTools(payload map[string]any) int {
	tools, ok := payload["tools"].([]any)
	if !ok || len(tools) == 0 || hasToolReference(payload["messages"]) {
		return 0
	}
	seen := make(map[string]bool, len(tools))
	var deferred []map[string]any
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			return 0
		}
		name, _ := tool["name"].(string)
		if strings.TrimSpace(name) == "" || seen[name] {
			return 0
		}
		seen[name] = true
		if kind, exists := tool["type"]; exists && kind != "custom" {
			return 0
		}
		schema, ok := tool["input_schema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			return 0
		}
		if value, exists := tool["defer_loading"]; exists {
			flag, ok := value.(bool)
			if !ok {
				return 0
			}
			if flag {
				deferred = append(deferred, tool)
			}
		}
	}
	for _, tool := range deferred {
		delete(tool, "defer_loading")
	}
	return len(deferred)
}

func hasToolReference(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		if value["type"] == "tool_reference" || value["type"] == "tool_search_tool_result" {
			return true
		}
		for _, child := range value {
			if hasToolReference(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if hasToolReference(child) {
				return true
			}
		}
	}
	return false
}
