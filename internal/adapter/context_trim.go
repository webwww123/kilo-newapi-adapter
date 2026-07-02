package adapter

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

const contextTrimOverheadTokens = 2048

type contextTrimProfile struct {
	ContextLimit   int
	SafetyMargin   int
	MaxInputTokens int
}

type contextTrimStats struct {
	Model          string
	TrimmedItems   int
	TruncatedItems int
	OutputClamped  bool
}

func (s contextTrimStats) changed() bool {
	return s.TrimmedItems > 0 || s.TruncatedItems > 0 || s.OutputClamped
}

var freeContextTrimProfiles = map[string]contextTrimProfile{
	"kilo-auto/free":                                     {ContextLimit: 256000, SafetyMargin: 40000, MaxInputTokens: 200000},
	"poolside/laguna-m.1:free":                           {ContextLimit: 262144, SafetyMargin: 40000, MaxInputTokens: 200000},
	"stepfun/step-3.7-flash:free":                        {ContextLimit: 262144, SafetyMargin: 0, MaxInputTokens: 220000},
	"cohere/north-mini-code:free":                        {ContextLimit: 256000, SafetyMargin: 40000, MaxInputTokens: 180000},
	"nvidia/nemotron-3.5-content-safety:free":            {ContextLimit: 128000, SafetyMargin: 40000, MaxInputTokens: 80000},
	"nvidia/nemotron-3-ultra-550b-a55b:free":             {ContextLimit: 1000000, SafetyMargin: 40000, MaxInputTokens: 900000},
	"nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free": {ContextLimit: 256000, SafetyMargin: 40000, MaxInputTokens: 200000},
	"poolside/laguna-xs-2.1:free":                        {ContextLimit: 262144, SafetyMargin: 40000, MaxInputTokens: 200000},
	"nvidia/nemotron-3-super-120b-a12b:free":             {ContextLimit: 1000000, SafetyMargin: 40000, MaxInputTokens: 900000},
	"openrouter/free":                                    {ContextLimit: 200000, SafetyMargin: 40000, MaxInputTokens: 150000},
}

func trimFreeModelContext(body []byte, upstreamPath string) ([]byte, contextTrimStats, error) {
	var stats contextTrimStats
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return body, stats, fmt.Errorf("invalid JSON body: %w", err)
	}
	payload, ok := raw.(map[string]any)
	if !ok {
		return body, stats, nil
	}

	model, _ := payload["model"].(string)
	model = strings.TrimPrefix(strings.TrimSpace(model), publicModelPrefix)
	stats.Model = model
	profile, ok := freeContextTrimProfiles[model]
	if !ok {
		return body, stats, nil
	}
	profile = normalizeContextTrimProfile(profile)

	path := strings.TrimRight(strings.TrimSpace(upstreamPath), "/")
	switch path {
	case "/chat/completions", "/messages":
		trimMessagePayload(payload, "messages", profile, false, &stats)
	case "/responses":
		trimResponsePayload(payload, profile, &stats)
	default:
		return body, stats, nil
	}
	if !stats.changed() {
		return body, stats, nil
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return body, stats, err
	}
	return out, stats, nil
}

func normalizeContextTrimProfile(profile contextTrimProfile) contextTrimProfile {
	if profile.SafetyMargin < 0 {
		profile.SafetyMargin = 0
	}
	if profile.MaxInputTokens < 0 {
		profile.MaxInputTokens = 0
	}
	return profile
}

func trimResponsePayload(payload map[string]any, profile contextTrimProfile, stats *contextTrimStats) {
	if _, ok := payload["input"].([]any); ok {
		trimMessagePayload(payload, "input", profile, true, stats)
		return
	}

	input, ok := payload["input"].(string)
	if !ok || input == "" {
		return
	}
	available := contextInputBudget(payload, profile, true, stats)
	available -= contextEstimateAnyTokens(payload["instructions"])
	if available < 64 {
		available = 64
	}
	if contextEstimateStringTokens(input) <= available {
		return
	}
	payload["input"] = trimStringPreservingEdgesToContextBudget(input, available)
	stats.TruncatedItems++
}

func trimMessagePayload(payload map[string]any, field string, profile contextTrimProfile, responses bool, stats *contextTrimStats) {
	items, ok := payload[field].([]any)
	if !ok || len(items) == 0 {
		return
	}
	available := contextInputBudget(payload, profile, responses, stats)
	if responses {
		available -= contextEstimateAnyTokens(payload["instructions"])
	} else {
		available -= contextEstimateAnyTokens(payload["system"])
	}
	if available < 512 {
		available = 512
	}

	if contextEstimateItemsTokens(items) <= available {
		return
	}

	kept := selectContextItemsForBudget(items, available)
	if contextEstimateItemsTokens(kept) > available {
		kept = truncateLatestUserContextItem(kept, available, stats)
	}
	if len(kept) < len(items) {
		stats.TrimmedItems += len(items) - len(kept)
	}
	payload[field] = kept
}

func contextInputBudget(payload map[string]any, profile contextTrimProfile, responses bool, stats *contextTrimStats) int {
	limit := profile.ContextLimit
	if limit <= 0 {
		return 0
	}
	outputBudget, outputField := outputBudgetFromPayload(payload, responses)
	if outputBudget <= 0 {
		outputBudget = 8192
	}
	if outputBudget > limit/2 {
		outputBudget = limit / 2
		if outputField == "" {
			if responses {
				outputField = "max_output_tokens"
			} else {
				outputField = "max_tokens"
			}
		}
		payload[outputField] = outputBudget
		stats.OutputClamped = true
	}

	available := limit - outputBudget - contextEstimateAnyTokens(payload["tools"]) - contextTrimOverheadTokens - profile.SafetyMargin
	if profile.MaxInputTokens > 0 && available > profile.MaxInputTokens {
		available = profile.MaxInputTokens
	}
	if available < 512 {
		available = 512
	}
	return available
}

func outputBudgetFromPayload(payload map[string]any, responses bool) (int, string) {
	if responses {
		if value := contextIntFromAny(payload["max_output_tokens"]); value > 0 {
			return value, "max_output_tokens"
		}
	}
	if value := contextIntFromAny(payload["max_tokens"]); value > 0 {
		return value, "max_tokens"
	}
	if value := contextIntFromAny(payload["max_completion_tokens"]); value > 0 {
		return value, "max_completion_tokens"
	}
	return 0, ""
}

type contextItemGroup struct {
	indexes []int
}

func selectContextItemsForBudget(items []any, budget int) []any {
	if len(items) == 0 {
		return items
	}
	latestUserIndex := -1
	for i, item := range items {
		if role := contextItemRole(item); role == "user" {
			latestUserIndex = i
		}
	}
	if latestUserIndex < 0 {
		latestUserIndex = len(items) - 1
	}

	selected := map[int]struct{}{latestUserIndex: {}}
	used := contextEstimateItemsTokens([]any{items[latestUserIndex]})
	addGroup := func(group contextItemGroup, force bool) bool {
		if len(group.indexes) == 0 {
			return false
		}
		groupItems := make([]any, 0, len(group.indexes))
		for _, index := range group.indexes {
			if index < 0 || index >= len(items) {
				continue
			}
			if _, ok := selected[index]; ok {
				continue
			}
			groupItems = append(groupItems, items[index])
		}
		cost := contextEstimateItemsTokens(groupItems)
		if cost == 0 {
			return true
		}
		if used+cost > budget && !force {
			return false
		}
		for _, index := range group.indexes {
			if index >= 0 && index < len(items) {
				selected[index] = struct{}{}
			}
		}
		used += cost
		return true
	}

	for i, item := range items {
		role := contextItemRole(item)
		if role != "system" && role != "developer" {
			continue
		}
		addGroup(contextItemGroup{indexes: []int{i}}, len(selected) == 1)
	}

	for _, group := range completedContextToolGroupsBefore(items, latestUserIndex) {
		addGroup(group, false)
	}

	for i := latestUserIndex - 1; i >= 0; i-- {
		if _, ok := selected[i]; ok {
			continue
		}
		role := contextItemRole(items[i])
		if role == "system" || role == "developer" || role == "tool" {
			continue
		}
		if contextItemHasToolCall(items[i]) {
			continue
		}

		group := contextItemGroup{indexes: []int{i}}
		if role == "assistant" {
			foundUser := false
			for j := i - 1; j >= 0; j-- {
				previousRole := contextItemRole(items[j])
				if previousRole == "system" || previousRole == "developer" {
					continue
				}
				if previousRole == "user" {
					group.indexes = []int{j, i}
					foundUser = true
					break
				}
			}
			if !foundUser {
				continue
			}
		}
		addGroup(group, false)
	}

	indexes := make([]int, 0, len(selected))
	for index := range selected {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	out := make([]any, 0, len(indexes))
	for _, index := range indexes {
		out = append(out, items[index])
	}
	return out
}

func completedContextToolGroupsBefore(items []any, endExclusive int) []contextItemGroup {
	if endExclusive > len(items) {
		endExclusive = len(items)
	}
	groups := make([]contextItemGroup, 0)
	for i := 0; i < endExclusive; i++ {
		callIDs := contextToolCallIDSet(items[i])
		if len(callIDs) == 0 {
			continue
		}
		indexes := []int{i}
		matched := map[string]struct{}{}
		for j := i + 1; j < endExclusive; j++ {
			if contextItemRole(items[j]) != "tool" {
				break
			}
			toolCallID := contextToolResultID(items[j])
			if _, ok := callIDs[toolCallID]; !ok {
				continue
			}
			indexes = append(indexes, j)
			matched[toolCallID] = struct{}{}
		}
		if len(matched) == len(callIDs) {
			groups = append(groups, contextItemGroup{indexes: indexes})
		}
	}
	for left, right := 0, len(groups)-1; left < right; left, right = left+1, right-1 {
		groups[left], groups[right] = groups[right], groups[left]
	}
	return groups
}

func contextItemRole(item any) string {
	message, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	if role, _ := message["role"].(string); strings.TrimSpace(role) != "" {
		return strings.TrimSpace(role)
	}
	switch strings.TrimSpace(fmt.Sprint(message["type"])) {
	case "function_call":
		return "assistant"
	case "function_call_output":
		return "tool"
	default:
		return ""
	}
}

func contextItemHasToolCall(item any) bool {
	return len(contextToolCallIDSet(item)) > 0
}

func contextToolCallIDSet(item any) map[string]struct{} {
	message, ok := item.(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]struct{}{}
	if strings.TrimSpace(fmt.Sprint(message["type"])) == "function_call" {
		if callID, _ := message["call_id"].(string); callID != "" {
			out[callID] = struct{}{}
		}
		return out
	}
	toolCalls, _ := message["tool_calls"].([]any)
	for _, item := range toolCalls {
		toolCall, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := toolCall["id"].(string); id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

func contextToolResultID(item any) string {
	message, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	if id, _ := message["tool_call_id"].(string); id != "" {
		return id
	}
	if id, _ := message["call_id"].(string); id != "" {
		return id
	}
	return ""
}

func truncateLatestUserContextItem(items []any, budget int, stats *contextTrimStats) []any {
	latestUserIndex := -1
	for i, item := range items {
		if role := contextItemRole(item); role == "user" {
			latestUserIndex = i
		}
	}
	if latestUserIndex < 0 {
		latestUserIndex = len(items) - 1
	}

	withoutLatest := append([]any{}, items[:latestUserIndex]...)
	if latestUserIndex+1 < len(items) {
		withoutLatest = append(withoutLatest, items[latestUserIndex+1:]...)
	}
	remaining := budget - contextEstimateItemsTokens(withoutLatest) - 16
	if remaining < 64 {
		remaining = 64
	}

	message, ok := items[latestUserIndex].(map[string]any)
	if !ok {
		return items
	}
	content, ok := message["content"].(string)
	if !ok {
		return items
	}
	if contextEstimateStringTokens(content) <= remaining {
		return items
	}
	message["content"] = trimStringPreservingEdgesToContextBudget(content, remaining)
	stats.TruncatedItems++
	return items
}

func contextEstimateItemsTokens(items []any) int {
	total := 0
	for _, item := range items {
		total += contextEstimateAnyTokens(item) + 4
	}
	return total
}

func contextEstimateAnyTokens(value any) int {
	switch v := value.(type) {
	case nil:
		return 0
	case string:
		return contextEstimateStringTokens(v)
	case []any:
		total := 2
		for _, item := range v {
			total += contextEstimateAnyTokens(item) + 1
		}
		return total
	case map[string]any:
		total := 2
		for key, item := range v {
			total += contextEstimateStringTokens(key) + contextEstimateAnyTokens(item) + 1
		}
		return total
	case float64, bool:
		return 1
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return 1
		}
		return contextEstimateStringTokens(string(data))
	}
}

func contextEstimateStringTokens(value string) int {
	ascii := 0
	nonASCII := 0
	for _, r := range value {
		if r <= 127 {
			ascii++
		} else {
			nonASCII++
		}
	}
	if ascii == 0 && nonASCII == 0 {
		return 0
	}
	return int(math.Ceil(float64(ascii)/2.0)) + nonASCII
}

func contextIntFromAny(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return 0
}

func trimStringPrefixToContextBudget(value string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if contextEstimateStringTokens(value) <= budget {
		return value
	}
	runes := []rune(value)
	low, high := 0, len(runes)
	best := 0
	for low <= high {
		mid := (low + high) / 2
		candidate := string(runes[:mid])
		if contextEstimateStringTokens(candidate) <= budget {
			best = mid
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	if best <= 0 {
		return ""
	}
	return string(runes[:best])
}

func trimStringSuffixToContextBudget(value string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if contextEstimateStringTokens(value) <= budget {
		return value
	}
	runes := []rune(value)
	low, high := 0, len(runes)
	best := 0
	for low <= high {
		mid := (low + high) / 2
		candidate := string(runes[len(runes)-mid:])
		if contextEstimateStringTokens(candidate) <= budget {
			best = mid
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	if best <= 0 {
		return ""
	}
	return string(runes[len(runes)-best:])
}

func trimStringPreservingEdgesToContextBudget(value string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if contextEstimateStringTokens(value) <= budget {
		return value
	}
	headBudget := budget / 4
	if headBudget < 16 && budget >= 32 {
		headBudget = 16
	}
	tailBudget := budget - headBudget
	return trimStringPrefixToContextBudget(value, headBudget) + trimStringSuffixToContextBudget(value, tailBudget)
}
