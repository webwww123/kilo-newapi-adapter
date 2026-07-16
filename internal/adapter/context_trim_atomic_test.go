package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestContextTrimKeepsNewestOpenAIToolTurnAtomically(t *testing.T) {
	withContextTrimProfile(t, "stepfun/step-3.7-flash:free", contextTrimProfile{ContextLimit: 5200})

	body := mustJSON(t, map[string]any{
		"model":      "kilo/stepfun/step-3.7-flash:free",
		"max_tokens": 512,
		"messages": []any{
			map[string]any{"role": "system", "content": "SYSTEM_ANCHOR"},
			map[string]any{"role": "user", "content": "TASK_ANCHOR use both completed reads"},
			map[string]any{
				"role":    "assistant",
				"content": "",
				"tool_calls": []any{
					map[string]any{"id": "call_a", "type": "function", "function": map[string]any{"name": "Read", "arguments": `{"file_path":"a.txt"}`}},
					map[string]any{"id": "call_b", "type": "function", "function": map[string]any{"name": "Read", "arguments": `{"file_path":"b.txt"}`}},
					map[string]any{"id": "call_c", "type": "function", "function": map[string]any{"name": "Read", "arguments": `{"file_path":"c.txt"}`}},
				},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_a", "content": []any{map[string]any{"type": "text", "text": "A_HEAD" + strings.Repeat("a", 14000) + "A_TAIL"}}},
			map[string]any{"role": "tool", "tool_call_id": "call_b", "content": []any{map[string]any{"type": "text", "text": "B_HEAD" + strings.Repeat("b", 14000) + "B_TAIL"}}},
			map[string]any{"role": "tool", "tool_call_id": "call_c", "content": []any{map[string]any{"type": "text", "text": "C_HEAD" + strings.Repeat("c", 14000) + "C_TAIL"}}},
		},
	})

	out, stats, err := trimFreeModelContext(body, "/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	items := contextTrimItems(t, out, "messages")
	if len(items) != 6 {
		t.Fatalf("messages after trim = %d, want complete parallel tool turn", len(items))
	}
	for _, marker := range []string{"TASK_ANCHOR", "call_a", "call_b", "call_c", "A_HEAD", "A_TAIL", "B_HEAD", "B_TAIL", "C_HEAD", "C_TAIL"} {
		if !contextItemsContain(items, marker) {
			t.Fatalf("marker %q was lost: %#v", marker, items)
		}
	}
	if got, wantMax := contextEstimateItemsTokens(items), 5200-512-contextTrimOverheadTokens; got > wantMax {
		t.Fatalf("estimated messages = %d, want <= %d", got, wantMax)
	}
	if stats.TruncatedItems < 3 {
		t.Fatalf("TruncatedItems = %d, want all oversized results truncated", stats.TruncatedItems)
	}
}

func TestContextTrimKeepsNewestAnthropicToolTurnAndOriginatingUser(t *testing.T) {
	withContextTrimProfile(t, "stepfun/step-3.7-flash:free", contextTrimProfile{ContextLimit: 7600})
	topLevelSystem := "SYSTEM_BEGIN " + strings.Repeat("s", 2000) + " SYSTEM_END"
	body := mustJSON(t, map[string]any{
		"model":      "kilo/stepfun/step-3.7-flash:free",
		"system":     topLevelSystem,
		"max_tokens": 512,
		"messages": []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "TASK_ANCHOR read both files"}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_old", "name": "Read", "input": map[string]any{"file_path": "old.txt"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_old", "content": "old result"}}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_a", "name": "Read", "input": map[string]any{"file_path": "a.txt"}},
				map[string]any{"type": "tool_use", "id": "toolu_b", "name": "Read", "input": map[string]any{"file_path": "b.txt"}},
				map[string]any{"type": "tool_use", "id": "toolu_c", "name": "Read", "input": map[string]any{"file_path": "c.txt"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_a", "content": []any{map[string]any{"type": "text", "text": "C_HEAD" + strings.Repeat("c", 14000) + "C_TAIL"}}},
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_b", "content": []any{map[string]any{"type": "text", "text": "D_HEAD" + strings.Repeat("d", 14000) + "D_TAIL"}}},
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_c", "content": []any{map[string]any{"type": "text", "text": "E_HEAD" + strings.Repeat("e", 14000) + "E_TAIL"}}},
			}},
		},
	})

	out, stats, err := trimFreeModelContext(body, "/messages")
	if err != nil {
		t.Fatal(err)
	}
	items := contextTrimItems(t, out, "messages")
	if len(items) != 3 {
		t.Fatalf("messages after trim = %d, want task + newest assistant/result", len(items))
	}
	for _, marker := range []string{"TASK_ANCHOR", "toolu_a", "toolu_b", "toolu_c", "C_HEAD", "C_TAIL", "D_HEAD", "D_TAIL", "E_HEAD", "E_TAIL"} {
		if !contextItemsContain(items, marker) {
			t.Fatalf("marker %q was lost: %#v", marker, items)
		}
	}
	wantMax := 7600 - 512 - contextEstimateAnyTokens(topLevelSystem) - contextTrimOverheadTokens
	if got := contextEstimateItemsTokens(items); got > wantMax {
		t.Fatalf("estimated messages = %d, want <= %d", got, wantMax)
	}
	if stats.TruncatedItems < 3 {
		t.Fatalf("TruncatedItems = %d, want all oversized result contents truncated", stats.TruncatedItems)
	}
}

func TestContextTrimKeepsNewestResponsesFunctionTurn(t *testing.T) {
	withContextTrimProfile(t, "stepfun/step-3.7-flash:free", contextTrimProfile{ContextLimit: 7600})
	body := mustJSON(t, map[string]any{
		"model":             "kilo/stepfun/step-3.7-flash:free",
		"max_output_tokens": 512,
		"input": []any{
			map[string]any{"role": "user", "content": "TASK_ANCHOR use both completed function outputs"},
			map[string]any{"type": "function_call", "call_id": "call_response_a", "name": "Read", "arguments": `{"file_path":"a.txt"}`},
			map[string]any{"type": "function_call", "call_id": "call_response_b", "name": "Read", "arguments": `{"file_path":"b.txt"}`},
			map[string]any{"type": "function_call", "call_id": "call_response_c", "name": "Read", "arguments": `{"file_path":"c.txt"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_response_a", "output": "RA_HEAD" + strings.Repeat("r", 14000) + "RA_TAIL"},
			map[string]any{"type": "function_call_output", "call_id": "call_response_b", "output": "RB_HEAD" + strings.Repeat("q", 14000) + "RB_TAIL"},
			map[string]any{"type": "function_call_output", "call_id": "call_response_c", "output": "RC_HEAD" + strings.Repeat("p", 14000) + "RC_TAIL"},
		},
	})

	out, stats, err := trimFreeModelContext(body, "/responses")
	if err != nil {
		t.Fatal(err)
	}
	items := contextTrimItems(t, out, "input")
	if len(items) != 7 {
		t.Fatalf("input after trim = %d, want complete parallel function turn", len(items))
	}
	for _, marker := range []string{"TASK_ANCHOR", "call_response_a", "call_response_b", "call_response_c", "RA_HEAD", "RA_TAIL", "RB_HEAD", "RB_TAIL", "RC_HEAD", "RC_TAIL"} {
		if !contextItemsContain(items, marker) {
			t.Fatalf("marker %q was lost: %#v", marker, items)
		}
	}
	if stats.TruncatedItems < 3 {
		t.Fatalf("TruncatedItems = %d, want all oversized function outputs truncated", stats.TruncatedItems)
	}
}

func TestContextTrimLeavesUnfitRequiredToolTurnUnchanged(t *testing.T) {
	withContextTrimProfile(t, "stepfun/step-3.7-flash:free", contextTrimProfile{ContextLimit: 2600})
	body := mustJSON(t, map[string]any{
		"model":      "kilo/stepfun/step-3.7-flash:free",
		"max_tokens": 512,
		"messages": []any{
			map[string]any{"role": "user", "content": "run the required tool"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_huge", "type": "function", "function": map[string]any{"name": "Huge", "arguments": `{"payload":"` + strings.Repeat("x", 12000) + `"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_huge", "content": "RESULT_HEAD " + strings.Repeat("r", 12000) + " RESULT_TAIL"},
		},
	})

	out, stats, err := trimFreeModelContext(body, "/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Fatalf("unfit request was partially mutated: input_bytes=%d output_bytes=%d", len(body), len(out))
	}
	if stats.TrimmedItems != 0 || stats.TruncatedItems != 0 {
		t.Fatalf("unfit request reported destructive trim stats: %#v", stats)
	}
}

func TestContextTrimLeavesUnknownResponsesItemsUnchanged(t *testing.T) {
	withContextTrimProfile(t, "stepfun/step-3.7-flash:free", contextTrimProfile{ContextLimit: 5000})
	body := mustJSON(t, map[string]any{
		"model":             "kilo/stepfun/step-3.7-flash:free",
		"max_output_tokens": 512,
		"input": []any{
			map[string]any{"role": "user", "content": "OLD " + strings.Repeat("x", 12000)},
			map[string]any{"role": "user", "content": "LATEST_TASK"},
			map[string]any{"type": "reasoning", "id": "reasoning_1", "summary": []any{map[string]any{"type": "summary_text", "text": "keep reasoning"}}},
		},
	})

	out, stats, err := trimFreeModelContext(body, "/responses")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Fatalf("unknown Responses items were partially trimmed: input_bytes=%d output_bytes=%d", len(body), len(out))
	}
	if stats.TrimmedItems != 0 || stats.TruncatedItems != 0 {
		t.Fatalf("unknown Responses request reported destructive trim stats: %#v", stats)
	}
}

func TestContextTrimLeavesStatefulOrUnpairedResponsesInputsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra map[string]any
	}{
		{name: "previous_response_id", extra: map[string]any{"previous_response_id": "resp_previous"}},
		{name: "conversation", extra: map[string]any{"conversation": "conv_previous"}},
		{name: "unpaired_outputs", extra: map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withContextTrimProfile(t, "stepfun/step-3.7-flash:free", contextTrimProfile{ContextLimit: 5000})
			payload := map[string]any{
				"model":             "kilo/stepfun/step-3.7-flash:free",
				"max_output_tokens": 512,
				"input": []any{
					map[string]any{"type": "function_call_output", "call_id": "call_a", "output": "A_HEAD" + strings.Repeat("a", 12000) + "A_TAIL"},
					map[string]any{"type": "function_call_output", "call_id": "call_b", "output": "B_HEAD" + strings.Repeat("b", 12000) + "B_TAIL"},
				},
			}
			for key, value := range tc.extra {
				payload[key] = value
			}
			body := mustJSON(t, payload)
			out, stats, err := trimFreeModelContext(body, "/responses")
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != string(body) {
				t.Fatalf("stateful/unpaired Responses input changed: input_bytes=%d output_bytes=%d", len(body), len(out))
			}
			if stats.TrimmedItems != 0 || stats.TruncatedItems != 0 {
				t.Fatalf("stateful/unpaired Responses input reported trim stats: %#v", stats)
			}
		})
	}
}

func TestContextTrimLeavesStatefulStringResponsesInputUnchanged(t *testing.T) {
	for _, key := range []string{"previous_response_id", "conversation"} {
		t.Run(key, func(t *testing.T) {
			withContextTrimProfile(t, "stepfun/step-3.7-flash:free", contextTrimProfile{ContextLimit: 3000})
			payload := map[string]any{
				"model":             "kilo/stepfun/step-3.7-flash:free",
				"max_output_tokens": 512,
				"input":             "HEAD " + strings.Repeat("x", 12000) + " TAIL",
				key:                 "external_state",
			}
			body := mustJSON(t, payload)
			out, stats, err := trimFreeModelContext(body, "/responses")
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != string(body) {
				t.Fatalf("stateful string Responses input changed: input_bytes=%d output_bytes=%d", len(body), len(out))
			}
			if stats.TrimmedItems != 0 || stats.TruncatedItems != 0 {
				t.Fatalf("stateful string Responses input reported trim stats: %#v", stats)
			}
		})
	}
}

func TestContextTrimLeavesMalformedResponsesPairingUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input []any
	}{
		{name: "missing_call_id", input: []any{
			map[string]any{"role": "user", "content": "TASK"},
			map[string]any{"type": "function_call", "name": "Read", "arguments": `{}`},
			map[string]any{"type": "function_call_output", "call_id": "call_a", "output": strings.Repeat("x", 12000)},
		}},
		{name: "call_without_output", input: []any{
			map[string]any{"role": "user", "content": "TASK"},
			map[string]any{"type": "function_call", "call_id": "call_a", "name": "Read", "arguments": `{}`},
			map[string]any{"role": "user", "content": strings.Repeat("x", 12000)},
		}},
		{name: "output_before_call", input: []any{
			map[string]any{"role": "user", "content": "TASK"},
			map[string]any{"type": "function_call_output", "call_id": "call_a", "output": strings.Repeat("x", 12000)},
			map[string]any{"type": "function_call", "call_id": "call_a", "name": "Read", "arguments": `{}`},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withContextTrimProfile(t, "stepfun/step-3.7-flash:free", contextTrimProfile{ContextLimit: 5000})
			body := mustJSON(t, map[string]any{
				"model":             "kilo/stepfun/step-3.7-flash:free",
				"max_output_tokens": 512,
				"input":             tc.input,
			})
			out, stats, err := trimFreeModelContext(body, "/responses")
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != string(body) {
				t.Fatalf("malformed Responses pairing changed: input_bytes=%d output_bytes=%d", len(body), len(out))
			}
			if stats.TrimmedItems != 0 || stats.TruncatedItems != 0 {
				t.Fatalf("malformed Responses pairing reported trim stats: %#v", stats)
			}
		})
	}
}

func TestContextTrimLeavesIncompleteChatAndAnthropicTurnsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name     string
		path     string
		messages []any
	}{
		{name: "chat_missing_result", path: "/chat/completions", messages: []any{
			map[string]any{"role": "user", "content": "TASK"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_a", "type": "function", "function": map[string]any{"name": "Read", "arguments": `{"payload":"` + strings.Repeat("x", 12000) + `"}`}},
			}},
		}},
		{name: "chat_orphan_result", path: "/chat/completions", messages: []any{
			map[string]any{"role": "user", "content": "TASK"},
			map[string]any{"role": "tool", "tool_call_id": "call_a", "content": strings.Repeat("x", 12000)},
		}},
		{name: "anthropic_missing_result", path: "/messages", messages: []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "TASK"}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_a", "name": "Read", "input": map[string]any{"payload": strings.Repeat("x", 12000)}}}},
		}},
		{name: "anthropic_orphan_result", path: "/messages", messages: []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "TASK"}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_a", "content": strings.Repeat("x", 12000)}}},
		}},
		{name: "chat_intervening_message", path: "/chat/completions", messages: []any{
			map[string]any{"role": "user", "content": "TASK"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_a", "type": "function", "function": map[string]any{"name": "Read", "arguments": `{}`}},
			}},
			map[string]any{"role": "assistant", "content": "intervening"},
			map[string]any{"role": "tool", "tool_call_id": "call_a", "content": strings.Repeat("x", 12000)},
		}},
		{name: "anthropic_split_results", path: "/messages", messages: []any{
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "TASK"}}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": "toolu_a", "name": "Read", "input": map[string]any{}},
				map[string]any{"type": "tool_use", "id": "toolu_b", "name": "Read", "input": map[string]any{}},
			}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_a", "content": strings.Repeat("a", 12000)}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_b", "content": strings.Repeat("b", 12000)}}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withContextTrimProfile(t, "stepfun/step-3.7-flash:free", contextTrimProfile{ContextLimit: 5000})
			body := mustJSON(t, map[string]any{
				"model":      "kilo/stepfun/step-3.7-flash:free",
				"max_tokens": 512,
				"messages":   tc.messages,
			})
			out, stats, err := trimFreeModelContext(body, tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != string(body) {
				t.Fatalf("incomplete tool history changed: input_bytes=%d output_bytes=%d", len(body), len(out))
			}
			if stats.TrimmedItems != 0 || stats.TruncatedItems != 0 {
				t.Fatalf("incomplete tool history reported trim stats: %#v", stats)
			}
		})
	}
}

func TestContextTrimLeavesResponsesResultsSeparatedByMessageUnchanged(t *testing.T) {
	withContextTrimProfile(t, "stepfun/step-3.7-flash:free", contextTrimProfile{ContextLimit: 6000})
	body := mustJSON(t, map[string]any{
		"model":             "kilo/stepfun/step-3.7-flash:free",
		"max_output_tokens": 512,
		"input": []any{
			map[string]any{"role": "user", "content": "TASK"},
			map[string]any{"type": "function_call", "call_id": "call_a", "name": "Read", "arguments": `{}`},
			map[string]any{"type": "function_call", "call_id": "call_b", "name": "Read", "arguments": `{}`},
			map[string]any{"type": "function_call_output", "call_id": "call_a", "output": strings.Repeat("a", 12000)},
			map[string]any{"role": "assistant", "content": "intervening"},
			map[string]any{"type": "function_call_output", "call_id": "call_b", "output": strings.Repeat("b", 12000)},
		},
	})
	out, stats, err := trimFreeModelContext(body, "/responses")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Fatalf("separated Responses outputs changed: input_bytes=%d output_bytes=%d", len(body), len(out))
	}
	if stats.TrimmedItems != 0 || stats.TruncatedItems != 0 {
		t.Fatalf("separated Responses outputs reported trim stats: %#v", stats)
	}
}

func TestContextTrimLeavesUnsupportedMultimodalContentUnchanged(t *testing.T) {
	withContextTrimProfile(t, "stepfun/step-3.7-flash:free", contextTrimProfile{ContextLimit: 3000})
	body := mustJSON(t, map[string]any{
		"model":      "kilo/stepfun/step-3.7-flash:free",
		"max_tokens": 512,
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + strings.Repeat("A", 12000)}},
			}},
		},
	})

	out, stats, err := trimFreeModelContext(body, "/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != string(body) {
		t.Fatalf("unsupported multimodal content was converted or partially trimmed: input_bytes=%d output_bytes=%d", len(body), len(out))
	}
	if stats.TrimmedItems != 0 || stats.TruncatedItems != 0 {
		t.Fatalf("unsupported multimodal request reported destructive trim stats: %#v", stats)
	}
}

func TestArrayValuedUserContentIsNotBlank(t *testing.T) {
	item := map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hello"}}}
	if isBlankContextUserItem(item) {
		t.Fatal("non-empty array-valued user content was treated as blank")
	}
}

func withContextTrimProfile(t *testing.T, model string, profile contextTrimProfile) {
	t.Helper()
	previous, existed := freeContextTrimProfiles[model]
	freeContextTrimProfiles[model] = profile
	t.Cleanup(func() {
		if existed {
			freeContextTrimProfiles[model] = previous
			return
		}
		delete(freeContextTrimProfiles, model)
	})
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func contextTrimItems(t *testing.T, body []byte, field string) []any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	items, ok := payload[field].([]any)
	if !ok {
		t.Fatalf("%s = %#v, want array", field, payload[field])
	}
	return items
}

func contextItemsContain(items []any, marker string) bool {
	return strings.Contains(string(mustMarshal(items)), marker)
}

func mustMarshal(value any) []byte {
	out, _ := json.Marshal(value)
	return out
}
