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
	"nvidia/nemotron-3-super-120b-a12b:free":             {ContextLimit: 262144, SafetyMargin: 40000, MaxInputTokens: 200000},
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
	if contextResponsesHasExternalState(payload) {
		return
	}
	available -= contextEstimateAnyTokens(payload["instructions"])
	if available < 64 {
		return
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
		if contextResponsesHasExternalState(payload) || !contextResponsesItemsSafeToTrim(items) {
			return
		}
	} else {
		available -= contextEstimateAnyTokens(payload["system"])
	}
	if !contextToolHistorySafeToTrim(items) {
		return
	}
	if available < 512 {
		// Fixed request fields already consume the useful context budget. Keep
		// the original history so the upstream returns an explicit context error
		// instead of receiving a stale, partially trimmed tool transcript.
		return
	}

	if contextEstimateItemsTokens(items) <= available {
		return
	}

	workingItems, ok := cloneContextJSONValue(items).([]any)
	if !ok {
		return
	}
	var trimStats contextTrimStats
	kept := selectContextItemsForBudget(workingItems, available)
	if contextEstimateItemsTokens(kept) > available {
		kept = truncateNewestContextToolResultsForBudget(kept, available, &trimStats)
	}
	if contextEstimateItemsTokens(kept) > available {
		kept = truncateLatestOrdinaryUserContextItem(kept, available, &trimStats)
	}
	if contextEstimateItemsTokens(kept) > available {
		return
	}
	if len(kept) < len(items) {
		trimStats.TrimmedItems += len(items) - len(kept)
	}
	payload[field] = kept
	stats.TrimmedItems += trimStats.TrimmedItems
	stats.TruncatedItems += trimStats.TruncatedItems
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

	forcedNewestToolGroup := false
	for _, group := range completedContextToolGroupsBefore(items, len(items)) {
		if !isCurrentContextToolGroup(items, group, latestUserIndex) {
			continue
		}
		if addGroup(group, !forcedNewestToolGroup) {
			forcedNewestToolGroup = true
		}
	}

	for _, group := range completedContextToolGroupsBefore(items, latestUserIndex) {
		addGroup(group, false)
	}

	for i := latestUserIndex - 1; i >= 0; i-- {
		if _, ok := selected[i]; ok {
			continue
		}
		role := contextItemRole(items[i])
		if role == "system" || role == "developer" {
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
					previous, _ := items[j].(map[string]any)
					if len(contextAnthropicToolResultIDSet(previous)) > 0 {
						continue
					}
					group.indexes = []int{j, i}
					foundUser = true
					break
				}
			}
			if !foundUser {
				continue
			}
		} else if role == "tool" {
			continue
		} else if role == "user" {
			message, _ := items[i].(map[string]any)
			if len(contextAnthropicToolResultIDSet(message)) > 0 {
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

func isCurrentContextToolGroup(items []any, group contextItemGroup, latestUserIndex int) bool {
	newest := newestContextGroupIndex(group)
	if newest >= latestUserIndex {
		return true
	}
	return latestUserIndex == len(items)-1 &&
		newest == latestUserIndex-1 &&
		isBlankContextUserItem(items[latestUserIndex])
}

func newestContextGroupIndex(group contextItemGroup) int {
	newest := -1
	for _, index := range group.indexes {
		if index > newest {
			newest = index
		}
	}
	return newest
}

func isBlankContextUserItem(item any) bool {
	message, ok := item.(map[string]any)
	if !ok || contextItemRole(message) != "user" {
		return false
	}
	switch content := message["content"].(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(content) == ""
	case []any:
		return len(content) == 0
	default:
		return false
	}
}

func contextResponsesItemsSafeToTrim(items []any) bool {
	callIndexes := make(map[string]int)
	outputIndexes := make(map[string]int)
	for index, item := range items {
		message, ok := item.(map[string]any)
		if !ok {
			return false
		}
		typeName, _ := message["type"].(string)
		typeName = strings.TrimSpace(typeName)
		switch typeName {
		case "function_call":
			ids := contextToolCallIDSet(message)
			if len(ids) != 1 {
				return false
			}
			for id := range ids {
				if _, exists := callIndexes[id]; exists {
					return false
				}
				callIndexes[id] = index
			}
			continue
		case "function_call_output":
			id := contextToolResultID(message)
			if id == "" {
				return false
			}
			if _, exists := outputIndexes[id]; exists {
				return false
			}
			outputIndexes[id] = index
			continue
		case "", "message":
			if contextItemRole(message) != "" {
				continue
			}
			return false
		default:
			return false
		}
	}
	if len(callIndexes) != len(outputIndexes) {
		return false
	}
	for id, callIndex := range callIndexes {
		outputIndex, ok := outputIndexes[id]
		if !ok || outputIndex <= callIndex {
			return false
		}
	}
	return true
}

func contextResponsesHasExternalState(payload map[string]any) bool {
	for _, key := range []string{"previous_response_id", "conversation"} {
		value, exists := payload[key]
		if !exists || value == nil {
			continue
		}
		if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
			continue
		}
		return true
	}
	return false
}

func contextToolHistorySafeToTrim(items []any) bool {
	openAICallIndexes := make(map[string]int)
	openAIResultIndexes := make(map[string]int)
	anthropicCallIndexes := make(map[string]int)
	anthropicResultIndexes := make(map[string]int)

	for index, item := range items {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := message["function_call"]; exists {
			return false
		}
		if contextItemRole(message) == "function" {
			return false
		}

		rawOpenAICalls := 0
		if strings.TrimSpace(fmt.Sprint(message["type"])) == "function_call" {
			rawOpenAICalls = 1
		} else if toolCalls, ok := message["tool_calls"].([]any); ok {
			rawOpenAICalls = len(toolCalls)
		}
		openAIIDs := contextToolCallIDSet(message)
		if rawOpenAICalls != len(openAIIDs) {
			return false
		}
		for id := range openAIIDs {
			if _, exists := openAICallIndexes[id]; exists {
				return false
			}
			openAICallIndexes[id] = index
		}

		if contextItemRole(message) == "tool" {
			id := contextToolResultID(message)
			if id == "" {
				return false
			}
			if _, exists := openAIResultIndexes[id]; exists {
				return false
			}
			openAIResultIndexes[id] = index
		}

		rawAnthropicCalls := 0
		rawAnthropicResults := 0
		if blocks, ok := message["content"].([]any); ok {
			for _, rawBlock := range blocks {
				block, ok := rawBlock.(map[string]any)
				if !ok {
					continue
				}
				switch strings.TrimSpace(fmt.Sprint(block["type"])) {
				case "tool_use":
					rawAnthropicCalls++
				case "tool_result":
					rawAnthropicResults++
				}
			}
		}
		anthropicCallIDs := contextAnthropicToolUseIDSet(message)
		anthropicResultIDs := contextAnthropicToolResultIDSet(message)
		if rawAnthropicCalls != len(anthropicCallIDs) || rawAnthropicResults != len(anthropicResultIDs) {
			return false
		}
		for id := range anthropicCallIDs {
			if _, exists := anthropicCallIndexes[id]; exists {
				return false
			}
			anthropicCallIndexes[id] = index
		}
		for id := range anthropicResultIDs {
			if _, exists := anthropicResultIndexes[id]; exists {
				return false
			}
			anthropicResultIndexes[id] = index
		}
	}

	if !contextToolPairIndexesMatch(openAICallIndexes, openAIResultIndexes) {
		return false
	}
	if !contextToolPairIndexesMatch(anthropicCallIndexes, anthropicResultIndexes) {
		return false
	}

	covered := make(map[int]struct{})
	for _, group := range completedContextToolGroupsBefore(items, len(items)) {
		for _, index := range group.indexes {
			covered[index] = struct{}{}
		}
	}
	for index, item := range items {
		message, _ := item.(map[string]any)
		isToolHistory := contextItemHasToolCall(item) ||
			contextItemRole(item) == "tool" ||
			len(contextAnthropicToolResultIDSet(message)) > 0
		if !isToolHistory {
			continue
		}
		if _, ok := covered[index]; !ok {
			return false
		}
	}
	return true
}

func contextToolPairIndexesMatch(callIndexes, resultIndexes map[string]int) bool {
	if len(callIndexes) != len(resultIndexes) {
		return false
	}
	for id, callIndex := range callIndexes {
		resultIndex, ok := resultIndexes[id]
		if !ok || resultIndex <= callIndex {
			return false
		}
	}
	return true
}

func completedContextToolGroupsBefore(items []any, endExclusive int) []contextItemGroup {
	groups := completedOpenAIContextToolGroupsBefore(items, endExclusive)
	groups = append(groups, completedAnthropicContextToolGroupsBefore(items, endExclusive)...)
	sort.SliceStable(groups, func(i, j int) bool {
		return newestContextGroupIndex(groups[i]) > newestContextGroupIndex(groups[j])
	})
	return groups
}

func completedOpenAIContextToolGroupsBefore(items []any, endExclusive int) []contextItemGroup {
	if endExclusive > len(items) {
		endExclusive = len(items)
	}
	groups := make([]contextItemGroup, 0)
	for i := 0; i < endExclusive; i++ {
		message, _ := items[i].(map[string]any)
		isResponsesCall := strings.TrimSpace(fmt.Sprint(message["type"])) == "function_call"
		if isResponsesCall && i > 0 {
			previous, _ := items[i-1].(map[string]any)
			if strings.TrimSpace(fmt.Sprint(previous["type"])) == "function_call" {
				continue
			}
		}
		callIDs := contextToolCallIDSet(items[i])
		if len(callIDs) == 0 {
			continue
		}
		indexes := []int{i}
		resultStart := i + 1
		if isResponsesCall {
			for j := i + 1; j < endExclusive; j++ {
				callItem, _ := items[j].(map[string]any)
				if strings.TrimSpace(fmt.Sprint(callItem["type"])) != "function_call" {
					break
				}
				for id := range contextToolCallIDSet(callItem) {
					callIDs[id] = struct{}{}
				}
				indexes = append(indexes, j)
				resultStart = j + 1
			}
		}
		matched := map[string]struct{}{}
		for j := resultStart; j < endExclusive; j++ {
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
	return groups
}

func completedAnthropicContextToolGroupsBefore(items []any, endExclusive int) []contextItemGroup {
	if endExclusive > len(items) {
		endExclusive = len(items)
	}
	groups := make([]contextItemGroup, 0)
	for i := 0; i < endExclusive; i++ {
		assistant, ok := items[i].(map[string]any)
		if !ok || contextItemRole(assistant) != "assistant" {
			continue
		}
		toolUseIDs := contextAnthropicToolUseIDSet(assistant)
		if len(toolUseIDs) == 0 {
			continue
		}
		for j := i + 1; j < endExclusive; j++ {
			resultMessage, ok := items[j].(map[string]any)
			if !ok || contextItemRole(resultMessage) != "user" {
				break
			}
			resultIDs := contextAnthropicToolResultIDSet(resultMessage)
			if len(resultIDs) == 0 {
				break
			}
			matchedAll := true
			for id := range toolUseIDs {
				if _, ok := resultIDs[id]; !ok {
					matchedAll = false
					break
				}
			}
			if matchedAll {
				indexes := []int{i, j}
				for k := i - 1; k >= 0; k-- {
					requestMessage, ok := items[k].(map[string]any)
					if !ok {
						continue
					}
					role := contextItemRole(requestMessage)
					if role == "system" || role == "developer" {
						continue
					}
					if role == "user" {
						if len(contextAnthropicToolResultIDSet(requestMessage)) > 0 {
							continue
						}
						indexes = append([]int{k}, indexes...)
						break
					}
					if role == "assistant" && len(contextAnthropicToolUseIDSet(requestMessage)) > 0 {
						continue
					}
					break
				}
				groups = append(groups, contextItemGroup{indexes: indexes})
			}
			break
		}
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
	message, _ := item.(map[string]any)
	return len(contextToolCallIDSet(item)) > 0 || len(contextAnthropicToolUseIDSet(message)) > 0
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

func contextAnthropicToolUseIDSet(message map[string]any) map[string]struct{} {
	blocks, _ := message["content"].([]any)
	out := make(map[string]struct{})
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok || strings.TrimSpace(fmt.Sprint(block["type"])) != "tool_use" {
			continue
		}
		if id, _ := block["id"].(string); id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

func contextAnthropicToolResultIDSet(message map[string]any) map[string]struct{} {
	blocks, _ := message["content"].([]any)
	out := make(map[string]struct{})
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok || strings.TrimSpace(fmt.Sprint(block["type"])) != "tool_result" {
			continue
		}
		if id, _ := block["tool_use_id"].(string); id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

func truncateNewestContextToolResultsForBudget(items []any, budget int, stats *contextTrimStats) []any {
	slots := contextToolResultSlots(items)
	if len(slots) == 0 {
		return items
	}
	emptyItems, ok := cloneContextJSONValue(items).([]any)
	if !ok {
		return items
	}
	for _, slot := range slots {
		if !setContextToolResultSlotValue(emptyItems, slot, "") {
			return items
		}
	}
	contentBudget := budget - contextEstimateItemsTokens(emptyItems)
	const minimumPerResult = 64
	if contentBudget < len(slots)*minimumPerResult {
		return items
	}
	perResult := contentBudget / len(slots)
	remainder := contentBudget % len(slots)
	for i, slot := range slots {
		value, ok := contextToolResultSlotValue(items, slot)
		if !ok {
			return items
		}
		resultBudget := perResult
		if i == len(slots)-1 {
			resultBudget += remainder
		}
		trimmed, changed := trimContextContentToBudget(value, resultBudget)
		if !changed {
			continue
		}
		if !setContextToolResultSlotValue(items, slot, trimmed) {
			return items
		}
		stats.TruncatedItems++
	}
	return items
}

type contextToolResultSlot struct {
	itemIndex  int
	blockIndex int
	field      string
}

func contextToolResultSlots(items []any) []contextToolResultSlot {
	slots := make([]contextToolResultSlot, 0)
	for itemIndex, item := range items {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch contextItemRole(message) {
		case "tool":
			field := "content"
			if strings.TrimSpace(fmt.Sprint(message["type"])) == "function_call_output" {
				field = "output"
			}
			if _, ok := message[field]; ok {
				slots = append(slots, contextToolResultSlot{itemIndex: itemIndex, blockIndex: -1, field: field})
			}
		case "user":
			blocks, _ := message["content"].([]any)
			for blockIndex, rawBlock := range blocks {
				block, ok := rawBlock.(map[string]any)
				if !ok || strings.TrimSpace(fmt.Sprint(block["type"])) != "tool_result" {
					continue
				}
				if _, ok := block["content"]; ok {
					slots = append(slots, contextToolResultSlot{itemIndex: itemIndex, blockIndex: blockIndex, field: "content"})
				}
			}
		}
	}
	return slots
}

func contextToolResultSlotValue(items []any, slot contextToolResultSlot) (any, bool) {
	if slot.itemIndex < 0 || slot.itemIndex >= len(items) {
		return nil, false
	}
	message, ok := items[slot.itemIndex].(map[string]any)
	if !ok {
		return nil, false
	}
	if slot.blockIndex < 0 {
		value, ok := message[slot.field]
		return value, ok
	}
	blocks, ok := message["content"].([]any)
	if !ok || slot.blockIndex >= len(blocks) {
		return nil, false
	}
	block, ok := blocks[slot.blockIndex].(map[string]any)
	if !ok {
		return nil, false
	}
	value, ok := block[slot.field]
	return value, ok
}

func setContextToolResultSlotValue(items []any, slot contextToolResultSlot, value any) bool {
	if slot.itemIndex < 0 || slot.itemIndex >= len(items) {
		return false
	}
	message, ok := items[slot.itemIndex].(map[string]any)
	if !ok {
		return false
	}
	if slot.blockIndex < 0 {
		message[slot.field] = value
		return true
	}
	blocks, ok := message["content"].([]any)
	if !ok || slot.blockIndex >= len(blocks) {
		return false
	}
	block, ok := blocks[slot.blockIndex].(map[string]any)
	if !ok {
		return false
	}
	block[slot.field] = value
	return true
}

func cloneContextJSONValue(value any) any {
	switch typed := value.(type) {
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = cloneContextJSONValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = cloneContextJSONValue(item)
		}
		return out
	default:
		return value
	}
}

func cloneContextMap(value map[string]any) map[string]any {
	out, _ := cloneContextJSONValue(value).(map[string]any)
	return out
}

func truncateLatestOrdinaryUserContextItem(items []any, budget int, stats *contextTrimStats) []any {
	latestUserIndex := -1
	for i, item := range items {
		message, ok := item.(map[string]any)
		if !ok || contextItemRole(message) != "user" || len(contextAnthropicToolResultIDSet(message)) > 0 {
			continue
		}
		latestUserIndex = i
	}
	if latestUserIndex < 0 {
		return items
	}

	message, ok := items[latestUserIndex].(map[string]any)
	if !ok {
		return items
	}
	content, ok := message["content"]
	if !ok {
		return items
	}
	emptyMessage := cloneContextMap(message)
	emptyMessage["content"] = ""
	withEmptyContent := append([]any{}, items...)
	withEmptyContent[latestUserIndex] = emptyMessage
	remaining := budget - contextEstimateItemsTokens(withEmptyContent)
	if remaining < 16 {
		remaining = 16
	}
	trimmed, changed := trimContextContentToBudget(content, remaining)
	if !changed {
		return items
	}
	message["content"] = trimmed
	stats.TruncatedItems++
	return items
}

func trimContextContentToBudget(value any, budget int) (any, bool) {
	if budget <= 0 {
		return "", true
	}
	if contextEstimateAnyTokens(value) <= budget {
		return value, false
	}

	switch typed := value.(type) {
	case string:
		return trimStringPreservingEdgesToContextBudget(typed, budget), true
	case []any:
		out := append([]any{}, typed...)
		for i := len(out) - 1; i >= 0; i-- {
			withoutItemCost := contextEstimateAnyTokens(out) - contextEstimateAnyTokens(out[i])
			itemBudget := budget - withoutItemCost - 1
			if itemBudget < 16 {
				itemBudget = 16
			}
			trimmed, changed := trimContextContentToBudget(out[i], itemBudget)
			if !changed {
				continue
			}
			out[i] = trimmed
			return out, true
		}
		return value, false
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = item
		}
		for _, key := range []string{"text", "content", "input", "output"} {
			raw, exists := out[key]
			if !exists {
				continue
			}
			withoutValueCost := contextEstimateAnyTokens(out) - contextEstimateAnyTokens(raw)
			valueBudget := budget - withoutValueCost - 1
			if valueBudget < 16 {
				valueBudget = 16
			}
			trimmed, changed := trimContextContentToBudget(raw, valueBudget)
			if !changed {
				continue
			}
			out[key] = trimmed
			return out, true
		}
		return value, false
	default:
		return value, false
	}
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
