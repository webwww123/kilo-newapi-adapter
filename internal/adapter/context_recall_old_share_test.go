package adapter

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestContextRecallAccountsForProtectedOldTaskBeforeRanking(t *testing.T) {
	items := []any{map[string]any{"role": "user", "content": strings.Repeat("Previous task statement. ", 50)}}
	add := func(n int, text string) {
		id := fmt.Sprintf("budget-call-%d", n)
		items = append(items,
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{map[string]any{
				"id": id, "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": id, "content": text})
	}
	add(0, strings.Repeat("needle_code ", 150))
	add(1, "needle_code implementation detail")
	for i := 2; i < 32; i++ {
		add(i, strings.Repeat("ordinary unrelated implementation output ", 40))
	}
	latest := len(items)
	items = append(items, map[string]any{"role": "user", "content": "Where is needle_code?"})
	for i := 32; i < 38; i++ {
		add(i, "current result unchanged")
	}
	baseline := append([]any{}, items[latest:]...)
	anchors := contextEstimateAnyTokens(items[0]) + contextEstimateAnyTokens(items[latest]) + 8
	largerPair := contextEstimateAnyTokens(items[1]) + contextEstimateAnyTokens(items[2]) + 8
	budget := anchors + 5*(largerPair+10)
	if contextEstimateItemsTokens(items) <= budget {
		t.Fatal("fixture must require trimming")
	}
	before := fmt.Sprint(items)
	engine := newContextRecallEngine()
	candidate, reason := engine.choose(context.Background(), map[string]any{"messages": items}, "chat", 50000, budget, nil, baseline)
	if reason != "selected" {
		t.Fatalf("a smaller matching pair fits without relaxing the old-share ceiling: %s", reason)
	}
	if before != fmt.Sprint(items) || !reflect.DeepEqual(candidate[len(candidate)-10:], items[len(items)-10:]) {
		t.Fatal("original or recent content changed")
	}
	contains := func(wanted any) bool {
		for _, value := range candidate {
			if reflect.DeepEqual(value, wanted) {
				return true
			}
		}
		return false
	}
	if !contains(items[0]) || !contains(items[3]) || !contains(items[4]) || contains(items[1]) || contains(items[2]) {
		t.Fatal("protected task or fitting evidence missing, or non-fitting pair selected")
	}
	added := 0
	for _, value := range candidate {
		known := false
		for _, old := range baseline {
			if reflect.DeepEqual(value, old) {
				known = true
				break
			}
		}
		if !known {
			added += contextEstimateAnyTokens(value) + 4
		}
	}
	if added > (budget-anchors)/5 || contextEstimateItemsTokens(candidate) > budget || !contextToolHistorySafeToTrim(candidate) {
		t.Fatal("old share, total budget, or tool pairing violated")
	}
	// A statement already present in the legacy input must not be charged a
	// second time. The higher-ranked, larger pair is then allowed to win.
	retained := append([]any{items[0]}, baseline...)
	candidate, reason = engine.choose(context.Background(), map[string]any{"messages": items}, "chat", 50000, budget, nil, retained)
	if reason != "selected" || !contains(items[1]) || !contains(items[2]) {
		t.Fatal("baseline task was charged as newly recalled information")
	}
	// If the mandatory older statement alone exceeds the cap, keep falling
	// back; counting it early must never waive the original protection.
	smallBudget := anchors + contextEstimateItemsTokens(baseline)
	if smallBudget < 512 || contextEstimateAnyTokens(items[0])+4 <= (smallBudget-anchors)/5 {
		t.Fatal("small-budget fixture does not exercise the protected-old cap")
	}
	if candidate, reason = engine.choose(context.Background(), map[string]any{"messages": items}, "chat", 50000, smallBudget, nil, baseline); candidate != nil || reason != "old_share_limit" {
		t.Fatal("mandatory old content exceeded its unchanged share limit")
	}
}
