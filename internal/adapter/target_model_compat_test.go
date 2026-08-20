package adapter

import "testing"

func TestTargetFreeModelsRaiseOnlyProvenLowOutputLimits(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	tests := []struct {
		name      string
		path      string
		body      string
		field     string
		wantLimit float64
	}{
		{
			name:      "hy3 messages raises low max tokens",
			path:      "/v1/messages",
			body:      `{"model":"kilo/tencent/hy3:free","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`,
			field:     "max_tokens",
			wantLimit: 256,
		},
		{
			name:      "hy3 chat remains unchanged",
			path:      "/v1/chat/completions",
			body:      `{"model":"kilo/tencent/hy3:free","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`,
			field:     "max_tokens",
			wantLimit: 64,
		},
		{
			name:      "dots responses raises low max output tokens",
			path:      "/v1/responses",
			body:      `{"model":"kilo/dots-studio/dots-3-note-preview:free","input":"hi","max_output_tokens":256}`,
			field:     "max_output_tokens",
			wantLimit: 512,
		},
		{
			name:      "dots chat remains unchanged",
			path:      "/v1/chat/completions",
			body:      `{"model":"kilo/dots-studio/dots-3-note-preview:free","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`,
			field:     "max_tokens",
			wantLimit: 64,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := len(*calls)
			rec := postJSON(t, handler, tc.path, tc.body)
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if len(*calls) != before+1 {
				t.Fatalf("calls=%d want=%d", len(*calls), before+1)
			}
			if got := (*calls)[before].Body[tc.field]; got != tc.wantLimit {
				t.Fatalf("%s=%#v want=%v body=%#v", tc.field, got, tc.wantLimit, (*calls)[before].Body)
			}
		})
	}
}

func TestHY3ForcedFunctionChoiceKeepsOnlySelectedTool(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	rec := postJSON(t, handler, "/v1/chat/completions", `{
		"model":"kilo/tencent/hy3:free",
		"messages":[{"role":"user","content":"use lookup"}],
		"tools":[
			{"type":"function","function":{"name":"search","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}}
		],
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"parallel_tool_calls":true,
		"max_tokens":1024
	}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("calls=%d want=1", len(*calls))
	}
	call := (*calls)[0].Body
	if call["model"] != "tencent/hy3:free" {
		t.Fatalf("model=%#v", call["model"])
	}
	if call["tool_choice"] != "required" {
		t.Fatalf("tool_choice=%#v want required", call["tool_choice"])
	}
	tools, ok := call["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools=%#v want one selected tool", call["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	function, _ := tool["function"].(map[string]any)
	if function["name"] != "lookup" {
		t.Fatalf("selected tool=%#v", tool)
	}
	if call["parallel_tool_calls"] != true {
		t.Fatalf("unrelated controls changed: %#v", call)
	}
}

func TestHY3AutoToolChoiceRemainsUnchanged(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	rec := postJSON(t, handler, "/v1/chat/completions", `{
		"model":"kilo/tencent/hy3:free",
		"messages":[{"role":"user","content":"use a tool if needed"}],
		"tools":[
			{"type":"function","function":{"name":"search","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}
		],
		"tool_choice":"auto"
	}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	call := (*calls)[0].Body
	if call["tool_choice"] != "auto" {
		t.Fatalf("tool_choice=%#v", call["tool_choice"])
	}
	if tools, ok := call["tools"].([]any); !ok || len(tools) != 2 {
		t.Fatalf("tools=%#v want unchanged", call["tools"])
	}
}

func TestSiblingForcedFunctionChoiceRemainsUnchanged(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	rec := postJSON(t, handler, "/v1/chat/completions", `{
		"model":"kilo/cohere/north-mini-code:free",
		"messages":[{"role":"user","content":"use lookup"}],
		"tools":[
			{"type":"function","function":{"name":"search","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}
		],
		"tool_choice":{"type":"function","function":{"name":"lookup"}}
	}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	call := (*calls)[0].Body
	if _, ok := call["tool_choice"].(map[string]any); !ok {
		t.Fatalf("sibling tool_choice changed: %#v", call["tool_choice"])
	}
	if tools, ok := call["tools"].([]any); !ok || len(tools) != 2 {
		t.Fatalf("sibling tools changed: %#v", call["tools"])
	}
}

func TestTargetFreeModelContextProfiles(t *testing.T) {
	tests := []struct {
		model string
		want  contextTrimProfile
	}{
		{
			model: "tencent/hy3:free",
			want:  contextTrimProfile{ContextLimit: 262144, SafetyMargin: 40000, MaxInputTokens: 200000, DefaultOutputTokens: 8192, MaxOutputTokens: 128000},
		},
		{
			model: "dots-studio/dots-3-note-preview:free",
			want:  contextTrimProfile{ContextLimit: 512000, SafetyMargin: 40000, MaxInputTokens: 400000, DefaultOutputTokens: 8192, MaxOutputTokens: 256000},
		},
	}
	for _, tc := range tests {
		if got, ok := freeContextTrimProfiles[tc.model]; !ok || got != tc.want {
			t.Fatalf("profile %s=%#v exists=%t want=%#v", tc.model, got, ok, tc.want)
		}
	}
}
