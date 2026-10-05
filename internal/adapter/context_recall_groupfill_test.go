package adapter

// Deterministic synthetic regression for the groupfill recall fix. The
// session is fabricated here (no user data): a long tool session whose
// baseline trim kept tool groups interleaved with dropped owner statements.
// The selector must restore every baseline item, never split a call from
// its result, keep the whole recent tail, and still recover the dropped
// needle turn through the evidence path.
import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func groupfillExamItems() []any {
	user := func(text string) map[string]any {
		return map[string]any{"role": "user", "content": text}
	}
	call := func(i int) map[string]any {
		return map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": fmt.Sprintf("call_%d", i), "type": "function",
				"function": map[string]any{"name": "read", "arguments": fmt.Sprintf(`{"path":"/var/log/svc-%d.log"}`, i)}},
		}}
	}
	result := func(i, lines int) map[string]any {
		return map[string]any{"role": "tool", "tool_call_id": fmt.Sprintf("call_%d", i),
			"content": strings.Repeat(fmt.Sprintf("svc-%d routine operational log line, status nominal\n", i), lines)}
	}
	needle := map[string]any{"role": "tool", "tool_call_id": "call_1",
		"content": "deployment token LANTERN-7741-XQ recorded\n" + strings.Repeat("svc-1 routine operational log line, status nominal\n", 40)}
	items := []any{
		map[string]any{"role": "system", "content": "Follow the current request. Keep identifiers exact."},
	}
	// The needle lives inside the dropped tool result (owner text is excluded
	// from a tool unit's scoring, so it must sit in a member item); the
	// question asks for it, so the evidence path must bring the unit back.
	items = append(items, user("task one finished."), call(1), needle)
	// A dropped owner statement whose tool group the baseline keeps: the
	// recall unit is mixed, which the buggy whole-unit baseline fill could
	// never restore. Positions 5,6 are the groupfill regression target.
	items = append(items, user("task two requested a routine status sweep."), call(2), result(2, 220))
	// Older turns the baseline drops outright.
	items = append(items, user("task three requested another sweep."), call(3), result(3, 220))
	items = append(items, user("task four requested a quiet sweep."), call(4), result(4, 220))
	// Six recent turns the baseline keeps in full.
	for i := 5; i <= 10; i++ {
		items = append(items, user(fmt.Sprintf("task %d requested a sweep.", i)), call(i), result(i, 180))
	}
	// The question and the in-progress tool turn answering it.
	items = append(items, user("what deployment token was recorded? answer with it only."), call(11), result(11, 30))
	return items
}

// groupfillPositions maps each output item back to its input position in
// order; fragment-modified copies count as present.
func groupfillPositions(kept, src []any) map[int]bool {
	out := make(map[int]bool, len(kept))
	at := 0
	for _, k := range kept {
		found := -1
		for at < len(src) {
			if reflect.DeepEqual(src[at], k) {
				found = at
				break
			}
			at++
		}
		if found >= 0 {
			out[found] = true
			at = found + 1
			continue
		}
		km, kok := k.(map[string]any)
		for j, s := range src {
			sm, sok := s.(map[string]any)
			if !sok || !kok {
				continue
			}
			match := true
			for key, va := range sm {
				if key == "content" {
					continue
				}
				if !reflect.DeepEqual(va, km[key]) {
					match = false
					break
				}
			}
			if match {
				for key, vb := range km {
					if key == "content" {
						continue
					}
					if !reflect.DeepEqual(vb, sm[key]) {
						match = false
						break
					}
				}
			}
			if match {
				out[j] = true
				break
			}
		}
	}
	return out
}

func TestContextRecallGroupfillInterleavedBaseline(t *testing.T) {
	items := groupfillExamItems()
	// The baseline keeps the system prompt, the mixed-unit group (positions
	// 5,6 whose owner at 4 was dropped), the six recent turns (13-30), and
	// the whole current turn (31-33). Older islands stay dropped.
	baselineIdx := []int{0, 5, 6}
	for i := 13; i <= 33; i++ {
		baselineIdx = append(baselineIdx, i)
	}
	var baseline []any
	for _, i := range baselineIdx {
		baseline = append(baseline, items[i])
	}
	est := func(idxs ...int) int {
		total := 0
		for _, i := range idxs {
			total += contextEstimateAnyTokens(items[i]) + 4
		}
		return total
	}
	budget := est(baselineIdx...) + est(1, 2, 3) + 400
	if total := estAllForTest(items); total <= budget {
		t.Fatalf("test setup: total %d <= budget %d, recall would not run", total, budget)
	}
	payload := map[string]any{"messages": items}
	engine := newContextRecallEngine()
	engine.timeout = 5 * time.Second
	metric := newContextRecallMetric()
	kept, reason := engine.chooseMetric(context.Background(), payload, "chat", 1<<20, budget, metric, baseline)
	if reason != "selected" {
		t.Fatalf("reason=%s want selected", reason)
	}
	pos := groupfillPositions(kept, items)
	// The baseline's mixed-unit group must be restored even though its owner
	// statement stays dropped.
	for _, i := range baselineIdx {
		if !pos[i] {
			t.Fatalf("baseline item %d dropped from recall output", i)
		}
	}
	if pos[4] {
		t.Fatalf("dropped owner statement unexpectedly restored: %d", 4)
	}
	// The needle turn returns through the evidence path.
	if !strings.Contains(contextRecallJoinedText(kept), "LANTERN-7741-XQ") {
		t.Fatalf("needle turn not recalled")
	}
	if !contextToolHistorySafeToTrim(kept) {
		t.Fatalf("output splits a tool call from its result")
	}
	if got := contextEstimateItemsTokens(kept); got > budget {
		t.Fatalf("output estimate %d exceeds budget %d", got, budget)
	}
	if want := min(50, len(baseline)); metric.tailKept != want {
		t.Fatalf("tailKept=%d want %d", metric.tailKept, want)
	}
	if metric.budgetUse <= 0 || metric.budgetUse > 110 {
		t.Fatalf("budgetUse=%d out of range", metric.budgetUse)
	}
}

func estAllForTest(items []any) int {
	total := 0
	for _, item := range items {
		total += contextEstimateAnyTokens(item) + 4
	}
	return total
}
