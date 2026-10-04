package adapter

// Offline coverage for the four kilo recall gaps: the laguna-s:free trim
// profile, thinking/image_url content blocks, the narrowed broad-query
// phrases, and the raised item cap. No model calls. Deterministic inputs.
import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func gapRecallRequest(engine *contextRecallEngine, protocol string, rawBytes, budget int) *contextRecallRequest {
	return &contextRecallRequest{
		control:        &contextRecallControl{engine: engine},
		ctx:            context.Background(),
		chosen:         true,
		fragmentChosen: true,
		event:          contextRecallEvent{Mode: "apply", Protocol: protocol, InputBytes: rawBytes},
	}
}

// poolside/laguna-s-2.1:free joins the sibling laguna trim profiles, so a 500k
// token request is trimmed for its 262144-token upstream instead of passed
// through and rejected. The mandatory document tail keeps the needle.
func TestContextRecallLagunaFreeOversize(t *testing.T) {
	const want = "FATAL: shard-7 vault marker LAGOON-77341 checksum mismatch"
	line := "2026-10-04T03:12:11Z INFO worker-3 processed batch 4815 latency=23ms ok\n"
	filler := strings.Repeat(line, 1000000/len(line)+1)
	var doc strings.Builder
	decoys := []float64{0.30, 0.42, 0.55, 0.66, 0.78}
	di := 0
	for pos := 0; pos+20000 <= len(filler); pos += 20000 {
		doc.WriteString(filler[pos : pos+20000])
		if di < len(decoys) && float64(pos+20000)/float64(len(filler)) >= decoys[di] {
			doc.WriteString("2026-10-04T03:12:11Z NOTICE transient 报错 recovered after retry\n")
			di++
		}
	}
	doc.WriteString("2026-10-04T03:59:58Z " + want + "\n")
	doc.WriteString(filler[:1500])
	question := "这个报错怎么修？"
	body := doc.String() + "\n" + question + "\n"
	if est := contextEstimateStringTokens(body); est < 480000 {
		t.Fatalf("fixture too small: %d", est)
	}
	payload := map[string]any{
		"model":      "kilo/poolside/laguna-s-2.1:free",
		"max_tokens": 8192,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a careful assistant."},
			map[string]any{"role": "user", "content": body},
		},
	}
	raw := mustJSON(t, payload)
	engine := newContextRecallEngine()
	engine.timeout = 10 * time.Second // isolate selection cost from the production deadline
	q := gapRecallRequest(engine, "chat", len(raw), 0)
	out, stats, err := trimFreeModelContext(raw, "/chat/completions", q)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.changed() || len(out) >= len(raw) {
		t.Fatalf("laguna-s:free request was not trimmed: stats=%#v", stats)
	}
	if q.event.Reason != "fragment_selected" || !q.event.Applied {
		t.Fatalf("recall did not repair the truncated request: %+v", q.event)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	text := contextRecallText(decoded["messages"])
	if !strings.Contains(text, want) {
		t.Fatal("trimmed request lost the needle")
	}
	if !strings.Contains(text, question) {
		t.Fatal("trimmed request lost the question")
	}
	if est := contextEstimateItemsTokens(decoded["messages"].([]any)); est > 200000 {
		t.Fatalf("trimmed estimate %d exceeds laguna max input", est)
	}
}

// Anthropic thinking/redacted_thinking blocks no longer poison the whole
// request: recall keeps or drops a message whole, never edits block contents
// or signatures, and the protected latest tool turn survives intact.
func TestContextRecallMessagesThinkingBlocks(t *testing.T) {
	thinkingText := "I should check the routine inspection logs before answering."
	signature := "sig-4f8b9c2d7e1a.placeholder"
	var items []any
	for n := 0; n < 44; n++ {
		text := "巡检记录 " + strings.Repeat("例行巡检读数正常，归档入库。", 40) + string(rune('a'+n%26))
		if n == 5 {
			text = "巡检记录：仓库保险柜暗号是 LAGOON-77341，登记后已封存。" + strings.Repeat("封存档案备注：保管完好。", 55)
		}
		items = append(items, map[string]any{"role": "user", "content": text})
		var content []any
		if n == 12 {
			content = []any{
				map[string]any{"type": "redacted_thinking", "data": "redacted-blob-9917"},
				map[string]any{"type": "text", "text": "巡检结果已记录。"},
			}
		} else {
			content = []any{
				map[string]any{"type": "thinking", "thinking": thinkingText, "signature": signature},
				map[string]any{"type": "text", "text": "巡检结果已记录，无异常。"},
			}
		}
		items = append(items, map[string]any{"role": "assistant", "content": content})
	}
	items = append(items, map[string]any{"role": "user", "content": "之前记录的仓库保险柜暗号是什么？只回答暗号。"})
	items = append(items, map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "thinking", "thinking": "Need the archived vault token for this question.", "signature": "sig-final-77"},
		map[string]any{"type": "tool_use", "id": "call_last", "name": "read", "input": map[string]any{"path": "vault/registry.txt"}},
	}})
	items = append(items, map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "tool_result", "tool_use_id": "call_last", "content": "registry read ok"},
	}})
	payload := map[string]any{
		"model":      "space-bunny-alpha",
		"system":     "You are a careful assistant.",
		"max_tokens": 8192,
		"messages":   items,
	}
	const budget = 9000
	if contextEstimateItemsTokens(items) <= budget {
		t.Fatalf("fixture must exceed budget: %d", contextEstimateItemsTokens(items))
	}
	profile := contextTrimProfile{ContextLimit: budget + 8192 + contextTrimOverheadTokens + contextEstimateAnyTokens(payload["system"])}

	var stats contextTrimStats
	q := gapRecallRequest(newContextRecallEngine(), "messages", len(mustJSON(t, payload)), budget)
	trimMessagePayload(payload, "messages", profile, false, &stats, q)
	if q.event.Reason != "selected" || !q.event.Applied {
		t.Fatalf("recall did not act on a thinking history: %s", q.event.Reason)
	}
	kept, ok := payload["messages"].([]any)
	if !ok || len(kept) == 0 || len(kept) >= len(items) {
		t.Fatalf("unexpected kept shape: %d of %d", len(kept), len(items))
	}

	// Every kept message is a whole original message, in order: no partial
	// content arrays, no edited thinking or signature fields.
	inputs := map[string]int{}
	for _, item := range items {
		inputs[string(mustJSON(t, item))]++
	}
	at := 0
	for _, item := range kept {
		encoded := string(mustJSON(t, item))
		if inputs[encoded] == 0 {
			t.Fatalf("candidate rewrote or invented a message: %.160s", encoded)
		}
		found := -1
		for i := at; i < len(items); i++ {
			if string(mustJSON(t, items[i])) == encoded {
				found = i
				break
			}
		}
		if found < 0 {
			t.Fatal("candidate reordered messages")
		}
		at = found + 1
	}
	countBlocks := func(list []any) int {
		count := 0
		for _, item := range list {
			m, _ := item.(map[string]any)
			blocks, _ := m["content"].([]any)
			for _, raw := range blocks {
				block, _ := raw.(map[string]any)
				if block["type"] == "thinking" || block["type"] == "redacted_thinking" {
					count++
				}
			}
		}
		return count
	}
	inThinking, outThinking := countBlocks(items), countBlocks(kept)
	if outThinking == 0 || outThinking > inThinking {
		t.Fatalf("thinking blocks=%d of %d", outThinking, inThinking)
	}
	// The needle evidence was recovered and the final thinking+tool_use /
	// tool_result pair survived untouched.
	joined := string(mustJSON(t, kept))
	if !strings.Contains(joined, "LAGOON-77341") {
		t.Fatal("recall dropped the old needle")
	}
	lastAssistant, lastUser := string(mustJSON(t, items[len(items)-2])), string(mustJSON(t, items[len(items)-1]))
	if !strings.Contains(joined, lastAssistant) || !strings.Contains(joined, lastUser) {
		t.Fatal("final thinking assistant + tool_result pair not preserved")
	}
	if contextEstimateItemsTokens(kept) > budget {
		t.Fatal("selection exceeded budget")
	}
}

// Broad-query rejection now only fires on phrases that name the whole
// history; ordinary wording like 总结/逐一/summarize no longer qualifies.
func TestContextRecallBroadQueryNarrowed(t *testing.T) {
	for _, text := range []string{
		"总结一下这个函数的问题",
		"请逐一检查 src 下的改动",
		"summarize the failing test output",
		"review every exported symbol",
	} {
		if contextRecallBroadQuery(text) {
			t.Fatalf("ordinary request still rejected as broad: %q", text)
		}
	}
	for _, text := range []string{
		"请总结全文",
		"汇总所有记录",
		"list all records",
		"the entire history of this repo",
		"review the whole conversation",
	} {
		if !contextRecallBroadQuery(text) {
			t.Fatalf("whole-history request not rejected: %q", text)
		}
	}

	var items []any
	for n := 0; n < 30; n++ {
		items = append(items,
			map[string]any{"role": "user", "content": "archive entry " + strings.Repeat("routine sampling notes ", 20)},
			map[string]any{"role": "assistant", "content": "recorded"})
	}
	const budget = 6000
	run := func(text string) string {
		payload := map[string]any{"messages": append(append([]any(nil), items...), map[string]any{"role": "user", "content": text})}
		working, _ := cloneContextJSONValue(payload["messages"]).([]any)
		baseline := selectContextItemsForBudget(working, budget)
		_, reason := newContextRecallEngine().choose(context.Background(), payload, "chat", len(mustJSON(t, payload)), budget, baseline)
		return reason
	}
	if reason := run("总结一下这个函数的问题，重点看 drain.rs 的错误处理。"); reason == "broad_query" {
		t.Fatal("ordinary summarize request still hits broad_query")
	}
	if reason := run("请总结全文。"); reason != "broad_query" {
		t.Fatalf("whole-document request lost its guard: %s", reason)
	}
}

// Long agent sessions above the old 4096-item cap are evaluated instead of
// rejected outright; at ~16000 items and ~4MB the selector either finishes
// inside the 250ms deadline or falls back exactly like before.
func TestContextRecallItemLimitRaised(t *testing.T) {
	var items []any
	for n := 0; n < 5000; n++ {
		items = append(items,
			map[string]any{"role": "user", "content": "routine check"},
			map[string]any{"role": "assistant", "content": "ok"})
	}
	payload := map[string]any{"messages": items}
	_, reason := newContextRecallEngine().choose(context.Background(), payload, "chat", len(mustJSON(t, payload)), 4000000)
	if reason == "item_limit" {
		t.Fatal("5000-item session still rejected by item_limit")
	}
}

// Measure production-timeout selection on a ~16000-item, ~4MB synthetic
// session: if it misses the 250ms deadline the engine reports "deadline" and
// the caller falls back to the legacy trim, exactly like an over-limit past.
func TestContextRecallItemLimitScaleMeasurement(t *testing.T) {
	var items []any
	for n := 0; n < 16384; n++ {
		role := "user"
		if n%2 == 1 {
			role = "assistant"
		}
		items = append(items, map[string]any{"role": role, "content": fmt.Sprintf("archive entry %05d ", n) + strings.Repeat("routine checkpoint observation ", 6)})
	}
	items[400].(map[string]any)["content"] = "archive entry 00400 hidden fact: vault marker ember-5521 recorded."
	items[16382].(map[string]any)["content"] = "What vault marker was recorded in archive entry 00400? Reply with the marker only."
	payload := map[string]any{"messages": items}
	raw := mustJSON(t, payload)
	if len(raw) < 3<<20 || len(raw) > 6<<20 {
		t.Fatalf("fixture size off target: %d bytes", len(raw))
	}
	const budget = 128000
	working, _ := cloneContextJSONValue(items).([]any)
	baseline := selectContextItemsForBudget(working, budget)

	engine := newContextRecallEngine()
	start := time.Now()
	kept, reason := engine.choose(context.Background(), payload, "chat", len(raw), budget, baseline)
	wall := time.Since(start)
	t.Logf("items=%d bytes=%d production_timeout=250ms reason=%s wall_ms=%d kept=%d", len(items), len(raw), reason, wall.Milliseconds(), len(kept))

	engine = newContextRecallEngine()
	engine.timeout = 60 * time.Second // measure the real selection cost
	start = time.Now()
	kept, reason = engine.choose(context.Background(), payload, "chat", len(raw), budget, baseline)
	wall = time.Since(start)
	t.Logf("items=%d bytes=%d unbounded reason=%s wall_ms=%d kept=%d", len(items), len(raw), reason, wall.Milliseconds(), len(kept))
	if reason != "selected" && reason != "deadline" {
		t.Fatalf("unexpected scale result: %s", reason)
	}
}
