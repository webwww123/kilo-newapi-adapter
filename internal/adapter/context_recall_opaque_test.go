package adapter

// Synthetic coverage for opaque payload blocks (thinking, media, files):
// whitelisted at the gate, invisible to scoring, never shrunk or rewritten,
// and never split from the tool group they ride in. All fixtures are
// fabricated here; no user data.
import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// opaquePositions maps each kept item to its source position using strict
// equality in input order. Atomic items must match byte-for-byte; a rewritten
// copy simply never maps, which the position assertions then catch.
func opaquePositions(kept, src []any) map[int]bool {
	out := make(map[int]bool, len(kept))
	at := 0
	for _, k := range kept {
		for at < len(src) && !reflect.DeepEqual(src[at], k) {
			at++
		}
		if at == len(src) {
			break
		}
		out[at] = true
		at++
	}
	return out
}

func opaqueEstimate(items []any, idxs ...int) int {
	total := 0
	for _, i := range idxs {
		total += contextEstimateAnyTokens(items[i]) + 4
	}
	return total
}

func opaqueThinkingCall(i int, thought string) map[string]any {
	return map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "thinking", "thinking": thought, "signature": fmt.Sprintf("sig-%d-%s", i, strings.Repeat("a", 24))},
		map[string]any{"type": "tool_use", "id": fmt.Sprintf("tu_%d", i), "name": "read",
			"input": map[string]any{"path": fmt.Sprintf("/var/log/svc-%d.log", i)}},
	}}
}

func opaqueAnthropicResult(i, lines int, extra string) map[string]any {
	return map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": fmt.Sprintf("tu_%d", i),
			"content": extra + strings.Repeat(fmt.Sprintf("svc-%d routine operational log line, status nominal\n", i), lines)},
	}}
}

// A long Anthropic session: every assistant tool call carries a thinking
// block, one old group hides the needle inside its thinking text only (it
// must never be recalled), and the needle group must come back whole.
func opaqueAnthropicItems() []any {
	user := func(text string) map[string]any {
		return map[string]any{"role": "user", "content": text}
	}
	items := []any{
		map[string]any{"role": "system", "content": "Follow the current request. Keep identifiers exact."},
	}
	// The needle group: its tool result names the token the question asks for.
	items = append(items, user("task one finished."),
		opaqueThinkingCall(1, "routine plan, nothing special"),
		opaqueAnthropicResult(1, 40, "deployment token LANTERN-7741-XQ recorded\n"))
	// The decoy group: the same needle phrase lives only inside the thinking
	// payload — once in "thinking" and once in a stray "text" field — both of
	// which must stay out of the relevance index.
	items = append(items, user("task two requested a sweep."),
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "thinking",
				"thinking":  "deployment token LANTERN-7741-XQ recorded here in the hidden trace",
				"text":      "deployment token LANTERN-7741-XQ recorded",
				"signature": "sig-2-" + strings.Repeat("b", 24)},
			map[string]any{"type": "tool_use", "id": "tu_2", "name": "read", "input": map[string]any{"path": "/var/log/svc-2.log"}},
		}},
		opaqueAnthropicResult(2, 60, ""))
	// Three more dropped groups plus one carrying a redacted thinking block.
	items = append(items, user("task three requested a sweep."), opaqueThinkingCall(3, "quiet reasoning"), opaqueAnthropicResult(3, 60, ""))
	items = append(items, user("task four requested a sweep."), opaqueThinkingCall(4, "more reasoning"), opaqueAnthropicResult(4, 60, ""))
	items = append(items, user("task five requested a sweep."),
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "redacted_thinking", "data": "REDACTED-5-payload-block"},
			map[string]any{"type": "tool_use", "id": "tu_5", "name": "read", "input": map[string]any{"path": "/var/log/svc-5.log"}},
		}},
		opaqueAnthropicResult(5, 60, ""))
	// Eighteen recent turns the baseline keeps in full.
	for i := 6; i <= 23; i++ {
		items = append(items, user(fmt.Sprintf("task %d requested a sweep.", i)),
			opaqueThinkingCall(i, "brief reasoning step"), opaqueAnthropicResult(i, 10, ""))
	}
	// The question and the in-progress tool turn answering it.
	items = append(items, user("what deployment token was recorded? answer with it only."),
		opaqueThinkingCall(24, "checking the logs"), opaqueAnthropicResult(24, 8, ""))
	return items
}

func TestContextRecallOpaqueThinkingToolGroups(t *testing.T) {
	items := opaqueAnthropicItems()
	// The baseline keeps the system prompt, the eighteen recent groups and
	// the current turn; the five older groups are dropped outright.
	baselineIdx := []int{0}
	for i := 16; i < len(items); i++ {
		baselineIdx = append(baselineIdx, i)
	}
	var baseline []any
	for _, i := range baselineIdx {
		baseline = append(baseline, items[i])
	}
	budget := opaqueEstimate(items, baselineIdx...) + opaqueEstimate(items, 1, 2, 3) + 1500
	if total := opaqueEstimate(items, func() []int {
		s := make([]int, len(items))
		for i := range s {
			s[i] = i
		}
		return s
	}()...); total <= budget {
		t.Fatalf("test setup: total %d <= budget %d, recall would not run", total, budget)
	}
	payload := map[string]any{"messages": items}
	engine := newContextRecallEngine()
	engine.timeout = 5 * time.Second
	stat := contextRecallTailStat{TailKept: -1, BudgetUse: -1}
	kept, reason := engine.choose(context.Background(), payload, "messages", 1<<20, budget, &stat, baseline)
	if reason != "selected" {
		t.Fatalf("reason=%s want selected", reason)
	}
	pos := opaquePositions(kept, items)
	// Every baseline item survives; the recent tail stat counts all fifty.
	for _, i := range baselineIdx {
		if !pos[i] {
			t.Fatalf("baseline item %d dropped from recall output", i)
		}
	}
	if stat.TailKept != 50 {
		t.Fatalf("tailKept=%d want 50 (baseline %d items)", stat.TailKept, len(baselineIdx))
	}
	// The needle group returned whole: owner statement, thinking call and
	// paired result are the same objects byte-for-byte.
	for _, i := range []int{1, 2, 3} {
		if !pos[i] {
			t.Fatalf("needle group member %d not recalled", i)
		}
	}
	// The decoy group stays out: its only query match sat inside a thinking
	// block, and opaque payload never feeds scoring.
	for _, i := range []int{4, 5, 6} {
		if pos[i] {
			t.Fatalf("decoy group member %d recalled via opaque payload scoring", i)
		}
	}
	// Nothing else old sneaks back either.
	for _, i := range []int{7, 8, 9, 10, 11, 12, 13, 14, 15} {
		if pos[i] {
			t.Fatalf("old group member %d unexpectedly restored", i)
		}
	}
	if !contextToolHistorySafeToTrim(kept) {
		t.Fatalf("output splits a tool call from its result")
	}
	if got := contextEstimateItemsTokens(kept); got > budget {
		t.Fatalf("output estimate %d exceeds budget %d", got, budget)
	}
	// Every opaque-bearing item in the output matched byte-for-byte already
	// (strict equality is the only mapping this test accepts); count them to
	// prove the check was exercised on real payloads.
	opaqueKept := 0
	for _, item := range kept {
		if contextRecallHasOpaque(item) {
			opaqueKept++
		}
	}
	if opaqueKept < 20 {
		t.Fatalf("only %d opaque items in output, expected the thinking groups", opaqueKept)
	}
}

// A long OpenAI chat session mixing multi-image, audio, file and video user
// messages with plain text turns. Media messages keep or drop whole.
func opaqueChatItems() []any {
	user := func(text string) map[string]any {
		return map[string]any{"role": "user", "content": text}
	}
	assistant := func(text string) map[string]any {
		return map[string]any{"role": "assistant", "content": text}
	}
	imageBlock := func(tag string) map[string]any {
		return map[string]any{"type": "image_url", "image_url": map[string]any{
			"url": fmt.Sprintf("data:image/png;base64,%s", strings.Repeat(tag, 64))}}
	}
	items := []any{
		map[string]any{"role": "system", "content": "Follow the current request. Keep identifiers exact."},
		map[string]any{"role": "user", "content": []any{
			imageBlock("AAAA"), imageBlock("BBBB"),
			map[string]any{"type": "text", "text": "floor plan one uploaded"}}},
		assistant("noted the first plan."),
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": strings.Repeat("UklGRg==", 64), "format": "wav"}},
			map[string]any{"type": "text", "text": "voice memo attached"}}},
		assistant("memo heard."),
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "file", "file": map[string]any{"file_data": strings.Repeat("JVBERi0=", 64), "filename": "spec.pdf"}},
			map[string]any{"type": "text", "text": "spec sheet attached"}}},
		assistant("spec logged."),
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "video_url", "video_url": map[string]any{"url": "https://example.invalid/clip.mp4"}},
			map[string]any{"type": "text", "text": "clip attached"}}},
		assistant("clip seen."),
		// The needle pair: a media user message whose text part names the
		// token, so the whole message comes back including its image.
		map[string]any{"role": "user", "content": []any{
			imageBlock("CCCC"),
			map[string]any{"type": "text", "text": "deployment token LANTERN-7741-XQ recorded in the diagram"}}},
		assistant("recorded."),
	}
	// Filler turns large enough to overflow the budget, none matching the
	// question.
	for i := 0; i < 6; i++ {
		items = append(items,
			user(fmt.Sprintf("interim note %d %s", i, strings.Repeat("routine filler ", 120))),
			assistant(fmt.Sprintf("acknowledged note %d %s", i, strings.Repeat("plain reply ", 120))))
	}
	// Twenty-four recent pairs the baseline keeps in full.
	for i := 0; i < 24; i++ {
		items = append(items,
			user(fmt.Sprintf("recent question %d", i)),
			assistant(fmt.Sprintf("recent answer %d", i)))
	}
	items = append(items, user("what deployment token was recorded? answer with it only."),
		assistant("looking it up now."))
	return items
}

func TestContextRecallOpaqueMediaAtomic(t *testing.T) {
	items := opaqueChatItems()
	// Baseline: system, the twenty-four recent pairs and the current turn.
	baselineIdx := []int{0}
	for i := len(items) - 50; i < len(items); i++ {
		baselineIdx = append(baselineIdx, i)
	}
	var baseline []any
	for _, i := range baselineIdx {
		baseline = append(baseline, items[i])
	}
	budget := opaqueEstimate(items, baselineIdx...) + opaqueEstimate(items, 9, 10) + 1500
	if total := opaqueEstimate(items, func() []int {
		s := make([]int, len(items))
		for i := range s {
			s[i] = i
		}
		return s
	}()...); total <= budget {
		t.Fatalf("test setup: total %d <= budget %d, recall would not run", total, budget)
	}
	payload := map[string]any{"messages": items}
	engine := newContextRecallEngine()
	engine.timeout = 5 * time.Second
	stat := contextRecallTailStat{TailKept: -1, BudgetUse: -1}
	kept, reason := engine.choose(context.Background(), payload, "chat", 1<<20, budget, &stat, baseline)
	if reason != "selected" {
		t.Fatalf("reason=%s want selected", reason)
	}
	pos := opaquePositions(kept, items)
	for _, i := range baselineIdx {
		if !pos[i] {
			t.Fatalf("baseline item %d dropped from recall output", i)
		}
	}
	if stat.TailKept != 50 {
		t.Fatalf("tailKept=%d want 50 (baseline %d items)", stat.TailKept, len(baselineIdx))
	}
	// The needle media message and its answer returned byte-for-byte.
	for _, i := range []int{9, 10} {
		if !pos[i] {
			t.Fatalf("needle media turn member %d not recalled whole", i)
		}
	}
	// Older media and filler turns were dropped by the baseline and never
	// scored.
	for _, i := range []int{1, 2, 3, 4, 5, 6, 7, 8, 11, 12, 13, 14, 15, 16} {
		if pos[i] {
			t.Fatalf("old turn member %d unexpectedly restored", i)
		}
	}
	if !contextToolHistorySafeToTrim(kept) {
		t.Fatalf("output splits a tool call from its result")
	}
	if got := contextEstimateItemsTokens(kept); got > budget {
		t.Fatalf("output estimate %d exceeds budget %d", got, budget)
	}
	// Strict mapping already proves every kept item is byte-identical; count
	// opaque items to prove the only media message back is the needle one.
	opaqueKept := 0
	for _, item := range kept {
		if contextRecallHasOpaque(item) {
			opaqueKept++
		}
	}
	if opaqueKept != 1 {
		t.Fatalf("opaque items in output=%d want 1 (the needle image message)", opaqueKept)
	}
}

func TestContextRecallOpaqueGateAndScoring(t *testing.T) {
	if !contextRecallSupported([]any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "thinking", "thinking": "trace", "signature": "sig"},
		map[string]any{"type": "redacted_thinking", "data": "blob"},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "AAAA"}},
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA"}},
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64,AA"},
		map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "AA", "format": "wav"}},
		map[string]any{"type": "audio", "data": "AA"},
		map[string]any{"type": "file", "file": map[string]any{"file_data": "AA", "filename": "x.pdf"}},
		map[string]any{"type": "input_file", "file_data": "AA", "filename": "x.pdf"},
		map[string]any{"type": "video_url", "video_url": map[string]any{"url": "https://example.invalid/v"}},
		map[string]any{"type": "text", "text": "hi"},
	}}}) {
		t.Fatalf("opaque block types rejected at the gate")
	}
	// Anything outside the whitelist is still refused.
	for _, kind := range []string{"document", "web_search_tool_result", "server_tool_use", "mcp_tool_use", "citation"} {
		if contextRecallSupported([]any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": kind, "data": "x"},
		}}}) {
			t.Fatalf("non-whitelisted block type %q accepted", kind)
		}
	}
	// Scoring reads the text parts only: thinking payload and signatures stay
	// out of the index even when embedded in an otherwise textual message.
	msg := map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "thinking", "thinking": "lantern hidden trace", "signature": "lantern signature"},
		map[string]any{"type": "text", "text": "visible words"},
	}}
	text := contextRecallText(msg)
	if strings.Contains(text, "lantern") || !strings.Contains(text, "visible words") {
		t.Fatalf("scoring text leaks opaque payload: %q", text)
	}
	if !contextRecallHasOpaque(msg) {
		t.Fatalf("opaque detector missed a thinking block")
	}
	if contextRecallHasOpaque(map[string]any{"role": "user", "content": "plain text"}) {
		t.Fatalf("opaque detector flagged a plain message")
	}
	// An opaque-bearing item can never produce a shrink plan.
	engine := newContextRecallEngine()
	big := map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + strings.Repeat("A", 9000)}},
		map[string]any{"type": "text", "text": strings.Repeat("long body paragraph\n\n", 400)},
	}}
	if plan := engine.planItemShrink(big); plan != nil {
		t.Fatalf("shrink plan created for an opaque item")
	}
}
