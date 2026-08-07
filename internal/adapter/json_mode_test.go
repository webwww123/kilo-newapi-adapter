package adapter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDowngradeFreeModelJSONModeRemovesFieldAndPreservesIntent(t *testing.T) {
	body := []byte(`{
		"model":"stepfun/step-3.7-flash:free",
		"messages":[
			{"role":"system","content":"Be concise."},
			{"role":"user","content":[{"type":"text","text":"give data"}]}
		],
		"response_format":{"type":"json_object"},
		"stream":true,
		"temperature":0.7
	}`)

	out, changed, err := downgradeFreeModelJSONMode(body, "/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("json_object request was not downgraded")
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["response_format"]; exists {
		t.Fatalf("response_format still forwarded: %s", out)
	}
	if payload["stream"] != true || payload["temperature"] != 0.7 {
		t.Fatalf("unrelated fields changed: %#v", payload)
	}
	messages := payload["messages"].([]any)
	system := messages[0].(map[string]any)
	if got := system["content"].(string); !strings.Contains(got, "Be concise.") || !strings.Contains(got, jsonObjectInstruction) {
		t.Fatalf("system instruction was not preserved and extended: %q", got)
	}
	user := messages[1].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("user message changed: %#v", user)
	}
}

func TestDowngradeFreeModelJSONModeAddsSystemMessageWhenMissing(t *testing.T) {
	body := []byte(`{"model":"stepfun/step-3.7-flash:free","messages":[{"role":"user","content":"give data"}],"response_format":{"type":"json_object"}}`)
	out, changed, err := downgradeFreeModelJSONMode(body, "/chat/completions")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	messages := payload["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" || messages[0].(map[string]any)["content"] != jsonObjectInstruction {
		t.Fatalf("JSON system instruction was not inserted: %#v", messages)
	}
}

func TestDowngradeFreeModelJSONModeLeavesOtherRequestsUnchanged(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{name: "other model", path: "/chat/completions", body: `{"model":"other/free","messages":[],"response_format":{"type":"json_object"}}`},
		{name: "json schema", path: "/chat/completions", body: `{"model":"stepfun/step-3.7-flash:free","messages":[],"response_format":{"type":"json_schema"}}`},
		{name: "responses protocol", path: "/responses", body: `{"model":"stepfun/step-3.7-flash:free","input":"x","response_format":{"type":"json_object"}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, changed, err := downgradeFreeModelJSONMode([]byte(tc.body), tc.path)
			if err != nil || changed || string(out) != tc.body {
				t.Fatalf("changed=%v err=%v out=%s", changed, err, out)
			}
		})
	}
}
