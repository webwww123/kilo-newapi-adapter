package adapter

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigFromEnvParsesKiloAutoRequestCompat(t *testing.T) {
	t.Setenv("KILO_AUTO_REQUEST_COMPAT_ENABLED", "true")
	if cfg := ConfigFromEnv(); !cfg.KiloAutoRequestCompatEnabled {
		t.Fatal("KILO_AUTO_REQUEST_COMPAT_ENABLED=true was not parsed")
	}
	t.Setenv("KILO_AUTO_REQUEST_COMPAT_ENABLED", "false")
	if cfg := ConfigFromEnv(); cfg.KiloAutoRequestCompatEnabled {
		t.Fatal("KILO_AUTO_REQUEST_COMPAT_ENABLED=false was not parsed")
	}
}

func TestNormalizeKiloAutoChatRequestRepairsRecoverableTools(t *testing.T) {
	body := []byte(`{
		"model":"kilo/kilo-auto/free",
		"messages":[{"role":"user","content":"use tools"}],
		"tools":[
			{"type":"function","name":"lookup","description":"Lookup data","strict":true,"parameters":{"type":"object","properties":null,"required":null}},
			{"type":"function","name":"search","input_schema":{"type":"object"}},
			{"type":"function","function":{},"name":"hybrid"},
			{"type":"function","function":{"name":"ping"}},
			{"type":"function"},
			{"type":"web_search"}
		]
	}`)

	out, stats, err := normalizeKiloAutoChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Matched || !stats.Changed || stats.FlatToolsWrapped != 3 || stats.ToolSchemasRepaired != 3 || stats.UnrecoverableTools != 1 {
		t.Fatalf("unexpected stats: %#v", stats)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	tools := payload["tools"].([]any)
	lookup := tools[0].(map[string]any)
	lookupFunction := lookup["function"].(map[string]any)
	if lookupFunction["name"] != "lookup" || lookupFunction["description"] != "Lookup data" || lookupFunction["strict"] != true {
		t.Fatalf("flat tool fields were not preserved: %#v", lookupFunction)
	}
	lookupParameters := lookupFunction["parameters"].(map[string]any)
	if lookupParameters["properties"] == nil || lookupParameters["required"] == nil {
		t.Fatalf("dirty parameter schema was not repaired: %#v", lookupParameters)
	}
	for _, key := range []string{"name", "description", "parameters", "input_schema", "strict"} {
		if _, exists := lookup[key]; exists {
			t.Fatalf("flat field %q remained on tool: %#v", key, lookup)
		}
	}
	searchFunction := tools[1].(map[string]any)["function"].(map[string]any)
	if searchFunction["name"] != "search" || searchFunction["parameters"].(map[string]any)["type"] != "object" {
		t.Fatalf("input_schema was not mapped: %#v", searchFunction)
	}
	hybridFunction := tools[2].(map[string]any)["function"].(map[string]any)
	if hybridFunction["name"] != "hybrid" || hybridFunction["parameters"].(map[string]any)["type"] != "object" {
		t.Fatalf("empty function object was not recovered from flat fields: %#v", hybridFunction)
	}
	pingParameters := tools[3].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if pingParameters["type"] != "object" {
		t.Fatalf("missing no-argument schema was not defaulted: %#v", pingParameters)
	}
	if _, exists := tools[4].(map[string]any)["function"]; exists {
		t.Fatalf("unrecoverable tool was invented: %#v", tools[4])
	}
	if tools[5].(map[string]any)["type"] != "web_search" {
		t.Fatalf("unrelated tool type changed: %#v", tools[5])
	}
}

func TestNormalizeKiloAutoChatRequestRemovesOnlyMatchedTrailingToolPlaceholder(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantChanged  bool
		wantRemoved  int
		wantMessages int
	}{
		{
			name:         "matching completed tool turn",
			body:         `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"use lookup"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"OK"},{"role":"user","content":""}]}`,
			wantChanged:  true,
			wantRemoved:  1,
			wantMessages: 3,
		},
		{
			name:         "multiple blank placeholders",
			body:         `{"model":"kilo-auto/free","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"OK"},{"role":"user","content":[]},{"role":"user","content":"  "}]}`,
			wantChanged:  true,
			wantRemoved:  2,
			wantMessages: 2,
		},
		{
			name:         "genuine empty user request",
			body:         `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":""}]}`,
			wantMessages: 1,
		},
		{
			name:         "orphan tool result",
			body:         `{"model":"kilo/kilo-auto/free","messages":[{"role":"tool","tool_call_id":"missing","content":"OK"},{"role":"user","content":""}]}`,
			wantMessages: 2,
		},
		{
			name:         "placeholder with semantic metadata",
			body:         `{"model":"kilo/kilo-auto/free","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"OK"},{"role":"user","content":"","name":"operator"}]}`,
			wantMessages: 3,
		},
		{
			name:         "stale matching call is not adjacent",
			body:         `{"model":"kilo/kilo-auto/free","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"user","content":"new turn"},{"role":"tool","tool_call_id":"call_1","content":"late result"},{"role":"user","content":""}]}`,
			wantMessages: 4,
		},
		{
			name:         "incomplete parallel tool results",
			body:         `{"model":"kilo/kilo-auto/free","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}},{"id":"call_2","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"one result"},{"role":"user","content":""}]}`,
			wantMessages: 3,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, stats, err := normalizeKiloAutoChatRequest([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if stats.Changed != tc.wantChanged || stats.EmptyToolPlaceholdersRemoved != tc.wantRemoved {
				t.Fatalf("stats=%#v", stats)
			}
			var payload map[string]any
			if err := json.Unmarshal(out, &payload); err != nil {
				t.Fatal(err)
			}
			if got := len(payload["messages"].([]any)); got != tc.wantMessages {
				t.Fatalf("messages=%d want=%d payload=%#v", got, tc.wantMessages, payload)
			}
		})
	}
}

func TestNormalizeKiloAutoChatRequestPromotesOnlyRecognizableFlatSchema(t *testing.T) {
	flat := []byte(`{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"return JSON"}],"response_format":{"type":"json_schema","json_schema":{"name":"probe","strict":true,"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}}}`)
	out, stats, err := normalizeKiloAutoChatRequest(flat)
	if err != nil {
		t.Fatal(err)
	}
	if !stats.Changed || !stats.FlatSchemaPromoted {
		t.Fatalf("flat Schema was not promoted: %#v", stats)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	container := payload["response_format"].(map[string]any)["json_schema"].(map[string]any)
	if container["name"] != "probe" || container["strict"] != true {
		t.Fatalf("Schema metadata changed: %#v", container)
	}
	schema := container["schema"].(map[string]any)
	if schema["type"] != "object" || schema["properties"] == nil || schema["required"] == nil {
		t.Fatalf("Schema root was not moved: %#v", schema)
	}
	if _, exists := container["type"]; exists {
		t.Fatalf("flat Schema keys remained in container: %#v", container)
	}

	unknown := []byte(`{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"return JSON"}],"response_format":{"type":"json_schema","json_schema":{"name":"probe","strict":true,"vendor_option":"x"}}}`)
	unknownOut, unknownStats, err := normalizeKiloAutoChatRequest(unknown)
	if err != nil {
		t.Fatal(err)
	}
	if unknownStats.Changed || string(unknownOut) != string(unknown) {
		t.Fatalf("unrecognized container was rewritten: stats=%#v body=%s", unknownStats, unknownOut)
	}

	other := []byte(`{"model":"kilo/other/free","tools":[{"type":"function","name":"lookup"}],"messages":[{"role":"user","content":"hi"}]}`)
	otherOut, otherStats, err := normalizeKiloAutoChatRequest(other)
	if err != nil {
		t.Fatal(err)
	}
	if otherStats.Matched || otherStats.Changed || string(otherOut) != string(other) {
		t.Fatalf("other model changed: stats=%#v body=%s", otherStats, otherOut)
	}
}

func TestKiloAutoRequestCompatHandlerRepairsThreeObservedShapes(t *testing.T) {
	var calls []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, payload)
		content := "OK"
		if len(calls) == 3 {
			content = `{"ok":true}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonCompatTestCompletion(t, kiloAutoFreeModel, content))
	}))
	defer upstream.Close()

	handler := NewHandler(Config{
		UpstreamBaseURL:              upstream.URL,
		HTTPClient:                   upstream.Client(),
		RequestLogDir:                t.TempDir(),
		KiloAutoReliabilityEnabled:   true,
		KiloAutoRequestCompatEnabled: true,
	})

	flatTool := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"reply OK"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{}}}],"tool_choice":"none"}`)
	if flatTool.Code != http.StatusOK || len(calls) != 1 {
		t.Fatalf("flat tool status=%d calls=%d body=%s", flatTool.Code, len(calls), flatTool.Body.String())
	}
	if _, ok := calls[0]["tools"].([]any)[0].(map[string]any)["function"].(map[string]any); !ok {
		t.Fatalf("flat tool did not reach upstream normalized: %#v", calls[0])
	}

	emptyPlaceholder := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"use lookup"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"OK"},{"role":"user","content":""}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":"none"}`)
	if emptyPlaceholder.Code != http.StatusOK || len(calls) != 2 || len(calls[1]["messages"].([]any)) != 3 {
		t.Fatalf("empty placeholder status=%d calls=%d upstream=%#v", emptyPlaceholder.Code, len(calls), calls)
	}

	flatSchema := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"return ok true"}],"response_format":{"type":"json_schema","json_schema":{"name":"probe","strict":true,"type":"object","additionalProperties":false,"properties":{"ok":{"type":"boolean","const":true}},"required":["ok"]}}}`)
	if flatSchema.Code != http.StatusOK || len(calls) != 3 || lingJSONTestResponseContent(t, flatSchema.Body.Bytes()) != `{"ok":true}` {
		t.Fatalf("flat Schema status=%d calls=%d body=%s", flatSchema.Code, len(calls), flatSchema.Body.String())
	}
	if _, exists := calls[2]["response_format"]; exists {
		t.Fatalf("normalized Schema was forwarded upstream instead of locally enforced: %#v", calls[2])
	}
}

func TestKiloAutoRequestCompatFeedsNormalizedMessagesIntoReliabilityRepair(t *testing.T) {
	var calls []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, payload)
		content := ""
		if len(calls) == 2 {
			content = "OK"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonCompatTestCompletion(t, kiloAutoFreeModel, content))
	}))
	defer upstream.Close()

	handler := NewHandler(Config{
		UpstreamBaseURL:              upstream.URL,
		HTTPClient:                   upstream.Client(),
		RequestLogDir:                t.TempDir(),
		KiloAutoReliabilityEnabled:   true,
		KiloAutoRequestCompatEnabled: true,
	})
	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"use lookup"},{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"OK"},{"role":"user","content":""}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":"none"}`)
	if rec.Code != http.StatusOK || len(calls) != 2 || lingJSONTestResponseContent(t, rec.Body.Bytes()) != "OK" {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(calls), rec.Body.String())
	}
	for callIndex, call := range calls {
		for messageIndex, raw := range call["messages"].([]any) {
			message, ok := raw.(map[string]any)
			if !ok || strings.ToLower(strings.TrimSpace(message["role"].(string))) != "user" {
				continue
			}
			if lingUserContentIsEmpty(message["content"]) {
				t.Fatalf("call %d retained empty user message %d: %#v", callIndex, messageIndex, call)
			}
		}
	}
}

func TestKiloAutoRequestCompatFlagDefaultsToOff(t *testing.T) {
	var call map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &call)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jsonCompatTestCompletion(t, kiloAutoFreeModel, "OK"))
	}))
	defer upstream.Close()
	handler := NewHandler(Config{UpstreamBaseURL: upstream.URL, HTTPClient: upstream.Client(), RequestLogDir: t.TempDir()})
	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"reply OK"}],"tools":[{"type":"function","name":"lookup"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	tool := call["tools"].([]any)[0].(map[string]any)
	if _, exists := tool["function"]; exists || tool["name"] != "lookup" {
		t.Fatalf("disabled compatibility changed request: %#v", call)
	}
}

func TestClassifyKiloAutoUpstream400(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantCategory  string
		wantParameter string
	}{
		{name: "missing function", body: `{"error":{"code":"invalid_request_error","message":"Invalid input: expected object, received undefined","param":"tools.0.function","type":"invalid_request_error"}}`, wantCategory: "missing_tool_function", wantParameter: "tool_function"},
		{name: "empty user", body: `{"error":{"code":"invalid_request_error","message":"user message must have content","param":"messages.3.content"}}`, wantCategory: "empty_user_content", wantParameter: "message_content"},
		{name: "paid auth", body: `{"error":{"code":"PAID_MODEL_AUTH_REQUIRED","message":"sign in"},"error_type":"paid_model_auth_required"}`, wantCategory: "paid_model_auth_required", wantParameter: "none"},
		{name: "tool parameters", body: `{"error":{"code":"invalid_request_error","message":"bad schema","param":"tools.2.function.parameters"}}`, wantCategory: "invalid_tool_schema", wantParameter: "tool_parameters"},
		{name: "response format", body: `{"error":{"code":"invalid_request_error","message":"bad schema","param":"response_format.json_schema"}}`, wantCategory: "invalid_response_format", wantParameter: "response_format"},
		{name: "non json", body: `bad gateway`, wantCategory: "non_json", wantParameter: "none"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			category, parameter := classifyKiloAutoUpstream400([]byte(tc.body))
			if category != tc.wantCategory || parameter != tc.wantParameter {
				t.Fatalf("category=%q parameter=%q", category, parameter)
			}
		})
	}
}

func TestKiloAuto400ClassifierDoesNotDependOnRawSecretText(t *testing.T) {
	body := `{"error":{"code":"invalid_request_error","message":"Invalid input Bearer secret-token https://internal.example/path","param":"tools.0.function"}}`
	category, parameter := classifyKiloAutoUpstream400([]byte(body))
	if category != "invalid_input" || parameter != "tool_function" {
		t.Fatalf("category=%q parameter=%q", category, parameter)
	}
	if strings.Contains(category+parameter, "secret") || strings.Contains(category+parameter, "internal") {
		t.Fatalf("classification leaked raw content: %q %q", category, parameter)
	}
}
