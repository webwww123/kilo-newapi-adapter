package adapter

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func runProxyRequestNormalizeChain(t *testing.T, body []byte, path string) []byte {
	t.Helper()
	var err error
	if body, _, err = normalizeKiloToolHistoryRequest(body, path); err != nil {
		t.Fatalf("tool history normalize: %v", err)
	}
	var rejection *lingRequestRejection
	if body, _, rejection, err = normalizeLingRequest(body, path); err != nil || rejection != nil {
		t.Fatalf("ling normalize: rejection=%+v err=%v", rejection, err)
	}
	if body, _, err = normalizeTargetFreeModelRequest(body, path); err != nil {
		t.Fatalf("target normalize: %v", err)
	}
	if body, _, err = downgradeFreeModelJSONMode(body, path); err != nil {
		t.Fatalf("json mode downgrade: %v", err)
	}
	return body
}

func apodexChatHistory(t *testing.T, body []byte) []any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	messages, ok := payload["messages"].([]any)
	if !ok {
		t.Fatalf("messages = %#v", payload["messages"])
	}
	return messages
}

func apodexChatToolCallBody(argumentsJSON string) []byte {
	return []byte(`{"model":"` + apodexMiniFreeModel + `","max_tokens":8192,"reasoning_effort":"minimal","messages":[` +
		`{"role":"user","content":"find the city"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":` + argumentsJSON + `}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"result body"},` +
		`{"role":"user","content":"thanks"}]}`)
}

func apodexChatToolArguments(t *testing.T, body []byte) (any, map[string]any) {
	t.Helper()
	messages := apodexChatHistory(t, body)
	call := messages[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	function := call["function"].(map[string]any)
	return function["arguments"], function
}

func TestApodexChatRepairsToolCallArguments(t *testing.T) {
	tests := []struct {
		name      string
		arguments string
		want      string
		wantRaw   string
		wantCount int
		identical bool
	}{
		{name: "object arguments serialize to a JSON string", arguments: `{"city":"Paris"}`, want: `{"city":"Paris"}`, wantCount: 1},
		{name: "array arguments serialize to a JSON string", arguments: `[{"city":"Paris"}]`, want: `[{"city":"Paris"}]`, wantCount: 1},
		{name: "empty arguments become an empty object", arguments: `""`, want: `{}`, wantCount: 1},
		{name: "blank arguments become an empty object", arguments: `"   "`, want: `{}`, wantCount: 1},
		{name: "invalid JSON arguments are wrapped verbatim", arguments: `"{\"city\":\"Par"`, wantRaw: `{"city":"Par`, wantCount: 1},
		{name: "valid JSON arguments pass through", arguments: `"{\"city\":\"Paris\"}"`, want: `{"city":"Paris"}`, identical: true},
		{name: "empty object string passes through", arguments: `"{}"`, want: `{}`, identical: true},
		{name: "non-object valid JSON strings pass through", arguments: `"[1,2]"`, want: `[1,2]`, identical: true},
		{name: "null arguments are untouched", arguments: `null`, identical: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := apodexChatToolCallBody(tc.arguments)
			out, stats, err := normalizeTargetFreeModelRequest(body, "/chat/completions")
			if err != nil {
				t.Fatal(err)
			}
			if stats.ToolArgsRepaired != tc.wantCount {
				t.Fatalf("ToolArgsRepaired=%d want=%d", stats.ToolArgsRepaired, tc.wantCount)
			}
			if tc.identical {
				if !bytes.Equal(out, body) {
					t.Fatalf("untouched request changed: %s", out)
				}
				return
			}
			got, function := apodexChatToolArguments(t, out)
			if function["name"] != "lookup" {
				t.Fatalf("function name changed: %#v", function)
			}
			if tc.wantRaw != "" {
				var wrapped map[string]any
				raw, ok := got.(string)
				if !ok || json.Unmarshal([]byte(raw), &wrapped) != nil || len(wrapped) != 1 || wrapped["_raw_arguments"] != tc.wantRaw {
					t.Fatalf("arguments=%#v, want wrapped %q", got, tc.wantRaw)
				}
			} else if got != tc.want {
				t.Fatalf("arguments=%#v want=%#v", got, tc.want)
			}
			messages := apodexChatHistory(t, out)
			call := messages[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
			if call["id"] != "call_1" || call["type"] != "function" {
				t.Fatalf("tool call identity changed: %#v", call)
			}
			result := messages[2].(map[string]any)
			if result["role"] != "tool" || result["tool_call_id"] != "call_1" || result["content"] != "result body" {
				t.Fatalf("tool result changed: %#v", result)
			}
		})
	}

	t.Run("missing arguments key is untouched", func(t *testing.T) {
		body := []byte(`{"model":"` + apodexMiniFreeModel + `","max_tokens":8192,"reasoning_effort":"minimal","messages":[` +
			`{"role":"user","content":"hi"},` +
			`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup"}}]},` +
			`{"role":"tool","tool_call_id":"call_1","content":"done"}]}`)
		out, stats, err := normalizeTargetFreeModelRequest(body, "/chat/completions")
		if err != nil || stats.ToolArgsRepaired != 0 || !bytes.Equal(out, body) {
			t.Fatalf("stats=%+v err=%v", stats, err)
		}
	})
}

func TestApodexConvertsMidConversationSystemMessages(t *testing.T) {
	tests := []struct {
		name      string
		messages  string
		wantRoles []string
		wantCount int
		check     func(t *testing.T, messages []any)
	}{
		{
			name: "leading system and developer stay, mid-conversation ones become user",
			messages: `[{"role":"system","content":"lead"},{"role":"developer","content":"dev lead"},` +
				`{"role":"user","content":"hi"},{"role":"system","content":"mid note"},` +
				`{"role":"developer","content":"mid dev"},{"role":"assistant","content":"ok"}]`,
			wantRoles: []string{"system", "developer", "user", "user", "user", "assistant"},
			wantCount: 2,
			check: func(t *testing.T, messages []any) {
				t.Helper()
				if messages[0].(map[string]any)["content"] != "lead" || messages[1].(map[string]any)["content"] != "dev lead" {
					t.Fatalf("leading system messages changed: %#v", messages[:2])
				}
				if messages[3].(map[string]any)["content"] != "[System note]\nmid note" {
					t.Fatalf("mid system content = %#v", messages[3].(map[string]any)["content"])
				}
				if messages[4].(map[string]any)["content"] != "[System note]\nmid dev" {
					t.Fatalf("mid developer content = %#v", messages[4].(map[string]any)["content"])
				}
			},
		},
		{
			name:      "array content gets a text block before the first text part",
			messages:  `[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}]`,
			wantRoles: []string{"user", "user"},
			wantCount: 1,
			check: func(t *testing.T, messages []any) {
				t.Helper()
				blocks := messages[1].(map[string]any)["content"].([]any)
				if len(blocks) != 3 || blocks[0].(map[string]any)["text"] != "[System note]\n" ||
					blocks[0].(map[string]any)["type"] != "text" ||
					blocks[1].(map[string]any)["text"] != "first" || blocks[2].(map[string]any)["text"] != "second" {
					t.Fatalf("content blocks = %#v", blocks)
				}
			},
		},
		{
			name:      "text block lands before the first text part, after non-text parts",
			messages:  `[{"role":"user","content":"hi"},{"role":"developer","content":[{"type":"image_url","image_url":{"url":"u"}},{"type":"text","text":"a"}]}]`,
			wantRoles: []string{"user", "user"},
			wantCount: 1,
			check: func(t *testing.T, messages []any) {
				t.Helper()
				blocks := messages[1].(map[string]any)["content"].([]any)
				if len(blocks) != 3 || blocks[0].(map[string]any)["type"] != "image_url" ||
					blocks[1].(map[string]any)["text"] != "[System note]\n" || blocks[2].(map[string]any)["text"] != "a" {
					t.Fatalf("content blocks = %#v", blocks)
				}
			},
		},
		{
			name:      "array without any text block gets one inserted first",
			messages:  `[{"role":"user","content":"hi"},{"role":"system","content":[{"type":"image_url","image_url":{"url":"u"}}]}]`,
			wantRoles: []string{"user", "user"},
			wantCount: 1,
			check: func(t *testing.T, messages []any) {
				t.Helper()
				blocks := messages[1].(map[string]any)["content"].([]any)
				if len(blocks) != 2 || blocks[0].(map[string]any)["type"] != "text" ||
					blocks[0].(map[string]any)["text"] != "[System note]\n" || blocks[1].(map[string]any)["type"] != "image_url" {
					t.Fatalf("content blocks = %#v", blocks)
				}
			},
		},
		{
			name:      "system message without content still converts",
			messages:  `[{"role":"user","content":"hi"},{"role":"system"}]`,
			wantRoles: []string{"user", "user"},
			wantCount: 1,
			check: func(t *testing.T, messages []any) {
				t.Helper()
				if messages[1].(map[string]any)["content"] != "[System note]\n" {
					t.Fatalf("content = %#v", messages[1].(map[string]any)["content"])
				}
			},
		},
		{
			name:      "consecutive user messages stay untouched",
			messages:  `[{"role":"user","content":"one"},{"role":"user","content":"two"}]`,
			wantRoles: []string{"user", "user"},
			wantCount: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"` + apodexMiniFreeModel + `","max_tokens":8192,"reasoning_effort":"minimal","messages":` + tc.messages + `}`)
			out, stats, err := normalizeTargetFreeModelRequest(body, "/chat/completions")
			if err != nil {
				t.Fatal(err)
			}
			if stats.MidSystemConverted != tc.wantCount {
				t.Fatalf("MidSystemConverted=%d want=%d", stats.MidSystemConverted, tc.wantCount)
			}
			if tc.wantCount == 0 {
				if !bytes.Equal(out, body) {
					t.Fatalf("untouched request changed: %s", out)
				}
				return
			}
			messages := apodexChatHistory(t, out)
			if len(messages) != len(tc.wantRoles) {
				t.Fatalf("messages=%d want=%d", len(messages), len(tc.wantRoles))
			}
			for i, want := range tc.wantRoles {
				if role := messages[i].(map[string]any)["role"]; role != want {
					t.Fatalf("messages[%d].role=%v want=%s", i, role, want)
				}
			}
			if tc.check != nil {
				tc.check(t, messages)
			}
		})
	}
}

func apodexMessagesToolUseBody(inputJSON string) []byte {
	return []byte(`{"model":"` + apodexMiniFreeModel + `","max_tokens":8192,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[` +
		`{"role":"user","content":"do it"},` +
		`{"role":"assistant","content":[{"type":"text","text":"calling"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":` + inputJSON + `}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}]}`)
}

func TestApodexRepairsMessagesToolUseInputs(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantInput any
		wantRaw   string
		wantCount int
		identical bool
	}{
		{name: "empty input becomes an empty object", input: `""`, wantInput: map[string]any{}, wantCount: 1},
		{name: "blank input becomes an empty object", input: `"  "`, wantInput: map[string]any{}, wantCount: 1},
		{name: "JSON object string parses to an object", input: `"{\"a\":1}"`, wantInput: map[string]any{"a": float64(1)}, wantCount: 1},
		{name: "non-object JSON string is wrapped", input: `"[1,2]"`, wantRaw: `[1,2]`, wantCount: 1},
		{name: "non-JSON string is wrapped", input: `"not json"`, wantRaw: "not json", wantCount: 1},
		{name: "object input passes through", input: `{"a":1}`, wantInput: map[string]any{"a": float64(1)}, identical: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := apodexMessagesToolUseBody(tc.input)
			out, stats, err := normalizeTargetFreeModelRequest(body, "/messages")
			if err != nil {
				t.Fatal(err)
			}
			if stats.ToolInputRepaired != tc.wantCount {
				t.Fatalf("ToolInputRepaired=%d want=%d", stats.ToolInputRepaired, tc.wantCount)
			}
			if tc.identical {
				if !bytes.Equal(out, body) {
					t.Fatalf("untouched request changed: %s", out)
				}
				return
			}
			messages := apodexChatHistory(t, out)
			blocks := messages[1].(map[string]any)["content"].([]any)
			if blocks[0].(map[string]any)["text"] != "calling" {
				t.Fatalf("text block changed: %#v", blocks[0])
			}
			toolUse := blocks[1].(map[string]any)
			if toolUse["id"] != "toolu_1" || toolUse["name"] != "lookup" {
				t.Fatalf("tool_use identity changed: %#v", toolUse)
			}
			if tc.wantRaw != "" {
				input, ok := toolUse["input"].(map[string]any)
				if !ok || len(input) != 1 || input["_raw_input"] != tc.wantRaw {
					t.Fatalf("input=%#v, want wrapped %q", toolUse["input"], tc.wantRaw)
				}
			} else if !reflect.DeepEqual(toolUse["input"], tc.wantInput) {
				t.Fatalf("input=%#v want=%#v", toolUse["input"], tc.wantInput)
			}
			result := messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)
			if result["type"] != "tool_result" || result["tool_use_id"] != "toolu_1" || result["content"] != "done" {
				t.Fatalf("tool result changed: %#v", result)
			}
		})
	}

	t.Run("missing input key is untouched", func(t *testing.T) {
		body := []byte(`{"model":"` + apodexMiniFreeModel + `","max_tokens":8192,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[` +
			`{"role":"user","content":"do it"},` +
			`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup"}]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}]}`)
		out, stats, err := normalizeTargetFreeModelRequest(body, "/messages")
		if err != nil || stats.ToolInputRepaired != 0 || !bytes.Equal(out, body) {
			t.Fatalf("stats=%+v err=%v", stats, err)
		}
	})
}

func TestApodexNormalizationLeavesOtherModelsByteIdentical(t *testing.T) {
	chatHistory := `[{"role":"user","content":"find the city"},` +
		`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}},{"id":"call_2","type":"function","function":{"name":"peek","arguments":{"x":1}}}]},` +
		`{"role":"tool","tool_call_id":"call_1","content":"r1"},{"role":"tool","tool_call_id":"call_2","content":"r2"},` +
		`{"role":"system","content":"late note"},{"role":"user","content":"thanks"}]`
	messagesHistory := `[{"role":"user","content":"do it"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":""}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}]`

	tests := []struct {
		name string
		path string
		body string
	}{
		{name: "space-bunny chat", path: "/chat/completions", body: `{"model":"` + spaceBunnyAlphaModel + `","max_tokens":8192,"reasoning_effort":"minimal","messages":` + chatHistory + `}`},
		{name: "space-bunny messages", path: "/messages", body: `{"model":"` + spaceBunnyAlphaModel + `","max_tokens":8192,"thinking":{"type":"enabled","budget_tokens":1024},"messages":` + messagesHistory + `}`},
		{name: "laguna chat", path: "/chat/completions", body: `{"model":"poolside/laguna-xs-2.1:free","max_tokens":64,"messages":` + chatHistory + `}`},
		{name: "laguna messages", path: "/messages", body: `{"model":"poolside/laguna-xs-2.1:free","max_tokens":64,"messages":` + messagesHistory + `}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			if out := runProxyRequestNormalizeChain(t, body, tc.path); !bytes.Equal(out, body) {
				t.Fatalf("other model request changed:\nin:  %s\nout: %s", body, out)
			}
		})
	}
}

func TestApodexCleanRequestStaysByteIdentical(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "chat",
			path: "/chat/completions",
			body: `{"model":"` + apodexMiniFreeModel + `","max_tokens":8192,"reasoning_effort":"minimal","messages":[` +
				`{"role":"system","content":"lead"},{"role":"developer","content":"dev"},{"role":"user","content":"hi"},` +
				`{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"city\":\"Paris\"}"}}]},` +
				`{"role":"tool","tool_call_id":"call_1","content":"done"},{"role":"user","content":"thanks"}]}`,
		},
		{
			name: "messages",
			path: "/messages",
			body: `{"model":"` + apodexMiniFreeModel + `","max_tokens":8192,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[` +
				`{"role":"user","content":"do it"},` +
				`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"a":1}}]},` +
				`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"done"}]}]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			if out := runProxyRequestNormalizeChain(t, body, tc.path); !bytes.Equal(out, body) {
				t.Fatalf("clean apodex request changed:\nin:  %s\nout: %s", body, out)
			}
		})
	}
}

func TestApodexHistoryRepairRunsBeforeContextTrim(t *testing.T) {
	withContextTrimProfile(t, apodexMiniFreeModel, contextTrimProfile{ContextLimit: 6000})

	body := mustJSON(t, map[string]any{
		"model": apodexMiniFreeModel,
		"messages": []any{
			map[string]any{"role": "user", "content": "OLD " + strings.Repeat("o", 20000)},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_old", "type": "function", "function": map[string]any{"name": "Read", "arguments": ""}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_old", "content": "OLDRES " + strings.Repeat("r", 20000)},
			map[string]any{"role": "user", "content": "TASK_ANCHOR latest"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "call_new", "type": "function", "function": map[string]any{"name": "Read", "arguments": ""}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_new", "content": "NEW_RESULT"},
		},
	})

	normalized, normStats, err := normalizeTargetFreeModelRequest(body, "/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if normStats.ToolArgsRepaired != 2 {
		t.Fatalf("ToolArgsRepaired=%d, want both calls repaired before trim", normStats.ToolArgsRepaired)
	}
	out, trimStats, err := trimFreeModelContext(normalized, "/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if trimStats.TrimmedItems != 3 {
		t.Fatalf("TrimmedItems=%d, want the over-budget history trimmed", trimStats.TrimmedItems)
	}
	items := contextTrimItems(t, out, "messages")
	if len(items) != 3 {
		t.Fatalf("messages after trim = %d, want task + newest tool group", len(items))
	}
	assistant := items[1].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("kept assistant tool_calls = %#v", calls)
	}
	function := calls[0].(map[string]any)["function"].(map[string]any)
	if function["arguments"] != "{}" {
		t.Fatalf("kept tool call arguments = %#v, want repaired {}", function["arguments"])
	}
	if calls[0].(map[string]any)["id"] != "call_new" {
		t.Fatalf("kept tool call id changed: %#v", calls[0])
	}
	result := items[2].(map[string]any)
	if result["role"] != "tool" || result["tool_call_id"] != "call_new" || result["content"] != "NEW_RESULT" {
		t.Fatalf("kept tool result changed: %#v", result)
	}
}
