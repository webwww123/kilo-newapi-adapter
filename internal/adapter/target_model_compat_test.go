package adapter

import "testing"

func TestTargetFreeModelsNormalizeOutputBudgetsAcrossProtocols(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	tests := []struct {
		name       string
		path       string
		body       string
		field      string
		wantLimit  float64
		absentKeys []string
	}{
		{
			name:       "hy3 chat preserves the highest client budget under the canonical field",
			path:       "/v1/chat/completions",
			body:       `{"model":"kilo/tencent/hy3:free","messages":[{"role":"user","content":"hi"}],"max_tokens":64,"max_completion_tokens":12000,"max_output_tokens":256}`,
			field:      "max_tokens",
			wantLimit:  12000,
			absentKeys: []string{"max_completion_tokens", "max_output_tokens"},
		},
		{
			name:       "dots chat adds the default floor when absent",
			path:       "/v1/chat/completions",
			body:       `{"model":"kilo/dots-studio/dots-3-note-preview:free","messages":[{"role":"user","content":"hi"}]}`,
			field:      "max_tokens",
			wantLimit:  8192,
			absentKeys: []string{"max_completion_tokens", "max_output_tokens"},
		},
		{
			name:       "hy3 messages raises a low budget",
			path:       "/v1/messages",
			body:       `{"model":"kilo/tencent/hy3:free","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`,
			field:      "max_tokens",
			wantLimit:  8192,
			absentKeys: []string{"max_completion_tokens", "max_output_tokens"},
		},
		{
			name:       "dots messages raises a low alias budget",
			path:       "/v1/messages",
			body:       `{"model":"kilo/dots-studio/dots-3-note-preview:free","messages":[{"role":"user","content":"hi"}],"max_completion_tokens":512}`,
			field:      "max_tokens",
			wantLimit:  8192,
			absentKeys: []string{"max_completion_tokens", "max_output_tokens"},
		},
		{
			name:       "hy3 responses preserves a higher alias budget",
			path:       "/v1/responses",
			body:       `{"model":"kilo/tencent/hy3:free","input":"hi","max_tokens":16000,"max_output_tokens":256}`,
			field:      "max_output_tokens",
			wantLimit:  16000,
			absentKeys: []string{"max_tokens", "max_completion_tokens"},
		},
		{
			name:       "dots responses raises a low budget",
			path:       "/v1/responses",
			body:       `{"model":"kilo/dots-studio/dots-3-note-preview:free","input":"hi","max_output_tokens":256}`,
			field:      "max_output_tokens",
			wantLimit:  8192,
			absentKeys: []string{"max_tokens", "max_completion_tokens"},
		},
		{
			name:       "inkling small chat raises a low budget",
			path:       "/v1/chat/completions",
			body:       `{"model":"kilo/thinkingmachines/inkling-small:free","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`,
			field:      "max_tokens",
			wantLimit:  8192,
			absentKeys: []string{"max_completion_tokens", "max_output_tokens"},
		},
		{
			name:       "inkling messages adds the default floor",
			path:       "/v1/messages",
			body:       `{"model":"kilo/thinkingmachines/inkling:free","messages":[{"role":"user","content":"hi"}]}`,
			field:      "max_tokens",
			wantLimit:  8192,
			absentKeys: []string{"max_completion_tokens", "max_output_tokens"},
		},
		{
			name:       "inkling responses preserves a higher budget",
			path:       "/v1/responses",
			body:       `{"model":"kilo/thinkingmachines/inkling:free","input":"hi","max_tokens":16000,"max_output_tokens":64}`,
			field:      "max_output_tokens",
			wantLimit:  16000,
			absentKeys: []string{"max_tokens", "max_completion_tokens"},
		},
		{
			name:       "space bunny chat raises a low budget",
			path:       "/v1/chat/completions",
			body:       `{"model":"kilo/stealth/space-bunny-alpha","messages":[{"role":"user","content":"hi"}],"max_tokens":64}`,
			field:      "max_tokens",
			wantLimit:  8192,
			absentKeys: []string{"max_completion_tokens", "max_output_tokens"},
		},
		{
			name:       "space bunny responses preserves a higher budget",
			path:       "/v1/responses",
			body:       `{"model":"kilo/stealth/space-bunny-alpha","input":"hi","max_tokens":16000,"max_output_tokens":64}`,
			field:      "max_output_tokens",
			wantLimit:  16000,
			absentKeys: []string{"max_tokens", "max_completion_tokens"},
		},
		{
			name:       "space bunny messages reserves a complete answer default",
			path:       "/v1/messages",
			body:       `{"model":"kilo/stealth/space-bunny-alpha","messages":[{"role":"user","content":"hi"}]}`,
			field:      "max_tokens",
			wantLimit:  32768,
			absentKeys: []string{"max_completion_tokens", "max_output_tokens"},
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
			for _, key := range tc.absentKeys {
				if _, exists := (*calls)[before].Body[key]; exists {
					t.Fatalf("unexpected %s in body=%#v", key, (*calls)[before].Body)
				}
			}
		})
	}
}

func TestTargetFreeModelsForceMinimumReasoningAcrossProtocols(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	for _, model := range []string{
		"kilo/tencent/hy3:free",
		"kilo/dots-studio/dots-3-note-preview:free",
		"kilo/thinkingmachines/inkling-small:free",
		"kilo/thinkingmachines/inkling:free",
		"kilo/stealth/space-bunny-alpha",
	} {
		t.Run(model+" chat", func(t *testing.T) {
			before := len(*calls)
			rec := postJSON(t, handler, "/v1/chat/completions", `{
				"model":"`+model+`",
				"messages":[{"role":"user","content":"hi"}],
				"reasoning_effort":"high",
				"reasoning":{"enabled":false,"effort":"high","max_tokens":32000},
				"thinking":{"type":"enabled","budget_tokens":32000}
			}`)
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			call := (*calls)[before].Body
			if call["reasoning_effort"] != "minimal" {
				t.Fatalf("reasoning_effort=%#v body=%#v", call["reasoning_effort"], call)
			}
			for _, key := range []string{"reasoning", "thinking"} {
				if _, exists := call[key]; exists {
					t.Fatalf("unexpected %s in body=%#v", key, call)
				}
			}
		})

		t.Run(model+" messages", func(t *testing.T) {
			before := len(*calls)
			rec := postJSON(t, handler, "/v1/messages", `{
				"model":"`+model+`",
				"messages":[{"role":"user","content":"hi"}],
				"reasoning_effort":"high",
				"reasoning":{"effort":"high"},
				"thinking":{"type":"enabled","budget_tokens":32000}
			}`)
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			call := (*calls)[before].Body
			thinking, ok := call["thinking"].(map[string]any)
			if !ok || thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(1024) {
				t.Fatalf("thinking=%#v body=%#v", call["thinking"], call)
			}
			for _, key := range []string{"reasoning_effort", "reasoning"} {
				if _, exists := call[key]; exists {
					t.Fatalf("unexpected %s in body=%#v", key, call)
				}
			}
		})

		t.Run(model+" responses", func(t *testing.T) {
			before := len(*calls)
			rec := postJSON(t, handler, "/v1/responses", `{
				"model":"`+model+`",
				"input":"hi",
				"reasoning_effort":"high",
				"reasoning":{"enabled":false,"effort":"high","max_tokens":32000},
				"thinking":{"type":"enabled","budget_tokens":32000}
			}`)
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			call := (*calls)[before].Body
			reasoning, ok := call["reasoning"].(map[string]any)
			if !ok || reasoning["effort"] != "minimal" || len(reasoning) != 1 {
				t.Fatalf("reasoning=%#v body=%#v", call["reasoning"], call)
			}
			for _, key := range []string{"reasoning_effort", "thinking"} {
				if _, exists := call[key]; exists {
					t.Fatalf("unexpected %s in body=%#v", key, call)
				}
			}
		})
	}
}

func TestTargetSiblingReasoningAndOutputRemainUnchanged(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	rec := postJSON(t, handler, "/v1/chat/completions", `{
		"model":"kilo/cohere/north-mini-code:free",
		"messages":[{"role":"user","content":"hi"}],
		"reasoning_effort":"high",
		"reasoning":{"max_tokens":32000},
		"max_tokens":64
	}`)
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	call := (*calls)[0].Body
	if call["reasoning_effort"] != "high" || call["max_tokens"] != float64(64) {
		t.Fatalf("sibling fields changed: %#v", call)
	}
	reasoning, ok := call["reasoning"].(map[string]any)
	if !ok || reasoning["max_tokens"] != float64(32000) {
		t.Fatalf("sibling reasoning changed: %#v", call["reasoning"])
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
		{
			model: "stealth/space-bunny-alpha",
			want:  contextTrimProfile{ContextLimit: 1000000, SafetyMargin: 40000, MaxInputTokens: 900000, DefaultOutputTokens: 8192, MaxOutputTokens: 524288},
		},
	}
	for _, tc := range tests {
		if got, ok := freeContextTrimProfiles[tc.model]; !ok || got != tc.want {
			t.Fatalf("profile %s=%#v exists=%t want=%#v", tc.model, got, ok, tc.want)
		}
	}
}

func TestNemotronFreeModelsForceReasoningOffAcrossProtocols(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	for _, model := range []string{
		"kilo/nvidia/nemotron-3-super-120b-a12b:free",
		"kilo/nvidia/nemotron-3-ultra-550b-a55b:free",
		"kilo/nvidia/nemotron-3.5-lightning:free",
	} {
		t.Run(model+" chat", func(t *testing.T) {
			before := len(*calls)
			rec := postJSON(t, handler, "/v1/chat/completions", `{
				"model":"`+model+`",
				"messages":[{"role":"user","content":"hi"}],
				"reasoning_effort":"high",
				"reasoning":{"enabled":true,"effort":"high"},
				"max_tokens":64
			}`)
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			call := (*calls)[before].Body
			if call["reasoning_effort"] != "none" {
				t.Fatalf("reasoning_effort=%#v want none, body=%#v", call["reasoning_effort"], call)
			}
			for _, key := range []string{"reasoning", "thinking"} {
				if _, exists := call[key]; exists {
					t.Fatalf("unexpected %s in body=%#v", key, call)
				}
			}
			if call["max_tokens"] != float64(64) {
				t.Fatalf("max_tokens should pass through unchanged, body=%#v", call)
			}
		})

		t.Run(model+" messages", func(t *testing.T) {
			before := len(*calls)
			rec := postJSON(t, handler, "/v1/messages", `{
				"model":"`+model+`",
				"messages":[{"role":"user","content":"hi"}],
				"thinking":{"type":"enabled","budget_tokens":32000},
				"reasoning_effort":"high"
			}`)
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			call := (*calls)[before].Body
			thinking, ok := call["thinking"].(map[string]any)
			if !ok || thinking["type"] != "disabled" || len(thinking) != 1 {
				t.Fatalf("thinking=%#v want disabled, body=%#v", call["thinking"], call)
			}
			if _, exists := call["reasoning_effort"]; exists {
				t.Fatalf("reasoning_effort should be removed, body=%#v", call)
			}
		})

		t.Run(model+" responses", func(t *testing.T) {
			before := len(*calls)
			rec := postJSON(t, handler, "/v1/responses", `{
				"model":"`+model+`",
				"input":"hi",
				"reasoning":{"effort":"high","max_tokens":32000}
			}`)
			if rec.Code != 200 {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			call := (*calls)[before].Body
			reasoning, ok := call["reasoning"].(map[string]any)
			if !ok || reasoning["effort"] != "none" || len(reasoning) != 1 {
				t.Fatalf("reasoning=%#v want effort=none, body=%#v", call["reasoning"], call)
			}
		})
	}
}
