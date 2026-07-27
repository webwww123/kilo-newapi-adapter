package adapter

import (
	"encoding/json"
	"testing"
)

func TestDropLingResponseFormatRemovesOnlyField(t *testing.T) {
	body := []byte(`{
		"model":"inclusionai/ling-3.0-flash:free",
		"messages":[{"role":"user","content":"give data"}],
		"response_format":{"type":"json_object"},
		"stream":true,
		"tools":[{"type":"function","function":{"name":"lookup"}}]
	}`)

	out, changed, err := dropLingResponseFormat(body, "/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("Ling request was not cleaned")
	}

	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["response_format"]; exists {
		t.Fatalf("response_format still present: %s", out)
	}
	if payload["stream"] != true {
		t.Fatalf("stream changed: %#v", payload)
	}
	if len(payload["messages"].([]any)) != 1 || len(payload["tools"].([]any)) != 1 {
		t.Fatalf("messages or tools changed: %#v", payload)
	}
}

func TestDropLingResponseFormatLeavesOtherSurfacesUnchanged(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{
			name: "other model",
			path: "/chat/completions",
			body: `{"model":"nvidia/other:free","messages":[],"response_format":{"type":"json_object"}}`,
		},
		{
			name: "responses protocol",
			path: "/responses",
			body: `{"model":"inclusionai/ling-3.0-flash:free","input":"x","response_format":{"type":"json_object"}}`,
		},
		{
			name: "missing response format",
			path: "/chat/completions",
			body: `{"model":"inclusionai/ling-3.0-flash:free","messages":[]}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, changed, err := dropLingResponseFormat([]byte(tc.body), tc.path)
			if err != nil || changed || string(out) != tc.body {
				t.Fatalf("changed=%v err=%v out=%s", changed, err, out)
			}
		})
	}
}
