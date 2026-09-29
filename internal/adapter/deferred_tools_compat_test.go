package adapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestDeferredToolsCompatPreservesDefinitionAndToolRoundTrip(t *testing.T) {
	requests := make(chan map[string]any, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		requests <- body
		tool := body["tools"].([]any)[0].(map[string]any)
		if tool["defer_loading"] == true {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"Deferred custom tools are not supported"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if len(body["messages"].([]any)) == 1 {
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read_file","input":{"path":"notes.txt"}}],"stop_reason":"tool_use"}`))
		} else {
			_, _ = w.Write([]byte(`{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"text","text":"File contents received"}],"stop_reason":"end_turn"}`))
		}
	}))
	defer upstream.Close()
	handler := newTestHandler(upstream.URL)
	tools := `[{"name":"read_file","description":"Read the requested file","defer_loading":true,"input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]`
	first := `{"model":"kilo/stealth/space-bunny-alpha","max_tokens":8192,"tools":` + tools + `,"messages":[{"role":"user","content":"Read notes.txt"}]}`
	response := postJSON(t, handler, "/v1/messages", first)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"name":"read_file"`) || !strings.Contains(response.Body.String(), `"path":"notes.txt"`) {
		t.Fatalf("tool request failed: status=%d response=%s", response.Code, response.Body.String())
	}
	firstUpstream := <-requests
	tool := firstUpstream["tools"].([]any)[0].(map[string]any)
	var original []map[string]any
	_ = json.Unmarshal([]byte(tools), &original)
	delete(original[0], "defer_loading")
	if !reflect.DeepEqual(tool, original[0]) {
		t.Fatalf("tool definition changed: %#v", tool)
	}
	history := `[{"role":"user","content":"Read notes.txt"},{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"read_file","input":{"path":"notes.txt"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"The file contains 42"}]}]`
	second := `{"model":"kilo/stealth/space-bunny-alpha","tools":` + tools + `,"messages":` + history + `}`
	response = postJSON(t, handler, "/v1/messages", second)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "File contents received") {
		t.Fatalf("tool result failed: status=%d response=%s", response.Code, response.Body.String())
	}
	secondUpstream := <-requests
	var expectedHistory any
	_ = json.Unmarshal([]byte(history), &expectedHistory)
	if !reflect.DeepEqual(secondUpstream["messages"], expectedHistory) {
		t.Fatalf("tool history changed: %#v", secondUpstream["messages"])
	}
}

func TestDeferredToolsCompatLeavesUnsupportedCasesUntouched(t *testing.T) {
	validTool := `{"name":"read_file","defer_loading":true,"input_schema":{"type":"object"}}`
	cases := []struct{ name, model, path, tools, messages string }{
		{"anthropic", "anthropic/claude-sonnet", "/messages", validTool, `[]`},
		{"other model", "thinkingmachines/inkling:free", "/messages", validTool, `[]`},
		{"chat", spaceBunnyAlphaModel, "/chat/completions", validTool, `[]`},
		{"responses", spaceBunnyAlphaModel, "/responses", validTool, `[]`},
		{"missing schema", spaceBunnyAlphaModel, "/messages", `{"name":"read_file","defer_loading":true}`, `[]`},
		{"missing name", spaceBunnyAlphaModel, "/messages", `{"defer_loading":true,"input_schema":{"type":"object"}}`, `[]`},
		{"partial definitions", spaceBunnyAlphaModel, "/messages", validTool + `,{"name":"unknown"}`, `[]`},
		{"search tool", spaceBunnyAlphaModel, "/messages", validTool + `,{"type":"tool_search_tool_regex_20251119","name":"tool_search"}`, `[]`},
		{"duplicate name", spaceBunnyAlphaModel, "/messages", validTool + `,` + validTool, `[]`},
		{"reference history", spaceBunnyAlphaModel, "/messages", validTool, `[{"role":"user","content":[{"type":"tool_result","content":[{"type":"tool_reference","tool_name":"read_file"}]}]}]`},
		{"invalid flag", spaceBunnyAlphaModel, "/messages", `{"name":"read_file","defer_loading":"true","input_schema":{"type":"object"}}`, `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"` + tc.model + `","tools":[` + tc.tools + `],"messages":` + tc.messages + `}`)
			out, stats, err := normalizeTargetFreeModelRequest(body, tc.path)
			if err != nil || stats.DeferredToolsExpanded != 0 {
				t.Fatalf("unexpected transformation: stats=%+v error=%v", stats, err)
			}
			var before, after map[string]any
			_ = json.Unmarshal(body, &before)
			_ = json.Unmarshal(out, &after)
			if !reflect.DeepEqual(before["tools"], after["tools"]) || !reflect.DeepEqual(before["messages"], after["messages"]) {
				t.Fatal("tools or history changed")
			}
		})
	}
}
