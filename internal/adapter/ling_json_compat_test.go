package adapter

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newLingJSONCompatTestHandler(t *testing.T, responder func(int, map[string]any) string) (http.Handler, *[]map[string]any, func()) {
	t.Helper()
	var calls []map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		calls = append(calls, payload)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responder(len(calls), payload))
	}))
	cfg := Config{
		UpstreamBaseURL: upstream.URL,
		HTTPClient:      upstream.Client(),
		RequestLogDir:   t.TempDir(),
	}
	return NewHandler(cfg), &calls, upstream.Close
}

func lingJSONTestCompletion(t *testing.T, content string) string {
	return jsonCompatTestCompletion(t, lingFlashFreeModel, content)
}

func jsonCompatTestCompletion(t *testing.T, model, content string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"id":      "chatcmpl-kilo-test",
		"object":  "chat.completion",
		"created": 1,
		"model":   model,
		"choices": []any{map[string]any{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": content,
			},
			"finish_reason": "stop",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestLingJSONCompatLeavesOrdinaryAndOtherModelsOnExistingPath(t *testing.T) {
	handler, calls, closeUpstream := newLingJSONCompatTestHandler(t, func(_ int, _ map[string]any) string {
		return lingJSONTestCompletion(t, "OK")
	})
	defer closeUpstream()

	ordinary := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/inclusionai/ling-3.0-flash:free","messages":[{"role":"user","content":"hello"}],"max_tokens":64}`)
	if ordinary.Code != http.StatusOK || lingJSONTestResponseContent(t, ordinary.Body.Bytes()) != "OK" {
		t.Fatalf("ordinary status=%d body=%s", ordinary.Code, ordinary.Body.String())
	}
	other := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/other/free","messages":[{"role":"user","content":"hello"}],"response_format":{"type":"json_object"}}`)
	if other.Code != http.StatusOK {
		t.Fatalf("other status=%d body=%s", other.Code, other.Body.String())
	}
	if len(*calls) != 2 {
		t.Fatalf("upstream calls=%d, want 2", len(*calls))
	}
	if (*calls)[0]["model"] != lingFlashFreeModel || (*calls)[0]["max_tokens"] != float64(64) {
		t.Fatalf("ordinary Ling request changed: %#v", (*calls)[0])
	}
	if _, exists := (*calls)[0]["response_format"]; exists {
		t.Fatalf("ordinary request gained response_format: %#v", (*calls)[0])
	}
	if _, exists := (*calls)[1]["response_format"]; !exists {
		t.Fatalf("other model response_format was intercepted: %#v", (*calls)[1])
	}
}

func TestLingJSONCompatRepairsEmptyCandidateWithSeparateOutputFloors(t *testing.T) {
	handler, calls, closeUpstream := newLingJSONCompatTestHandler(t, func(call int, _ map[string]any) string {
		if call == 1 {
			return lingJSONTestCompletion(t, "")
		}
		return lingJSONTestCompletion(t, `{"status":"ok","value":7}`)
	})
	defer closeUpstream()

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/inclusionai/ling-3.0-flash:free","stream":false,"temperature":0.7,"max_tokens":64,"response_format":{"type":"json_object"},"messages":[{"role":"system","content":"public identity policy"},{"role":"user","content":"The semantic result is status ok and value 7."}]}`)
	if rec.Code != http.StatusOK || lingJSONTestResponseContent(t, rec.Body.Bytes()) != `{"status":"ok","value":7}` {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 2 {
		t.Fatalf("upstream calls=%d, want 2", len(*calls))
	}
	first, second := (*calls)[0], (*calls)[1]
	if first["model"] != lingFlashFreeModel || first["stream"] != false || first["n"] != float64(1) || first["max_tokens"] != float64(lingJSONFirstOutputFloor) {
		t.Fatalf("first attempt controls wrong: %#v", first)
	}
	if _, exists := first["response_format"]; exists {
		t.Fatalf("first attempt forwarded response_format: %#v", first)
	}
	firstMessages := first["messages"].([]any)
	if !strings.Contains(firstMessages[len(firstMessages)-1].(map[string]any)["content"].(string), "FORMAT REQUIREMENT") {
		t.Fatalf("first format constraint missing: %#v", firstMessages)
	}
	if second["model"] != lingFlashFreeModel || second["stream"] != false || second["n"] != float64(1) || second["temperature"] != float64(0) || second["max_tokens"] != float64(lingJSONRepairOutputFloor) {
		t.Fatalf("repair controls wrong: %#v", second)
	}
	if _, exists := second["response_format"]; exists {
		t.Fatalf("Ling cleanup did not remove repair response_format: %#v", second)
	}
	secondMessages := second["messages"].([]any)
	if len(secondMessages) != 3 || secondMessages[0].(map[string]any)["content"] != "public identity policy" || secondMessages[1].(map[string]any)["role"] != "developer" {
		t.Fatalf("repair policy context wrong: %#v", secondMessages)
	}
	repairData := secondMessages[2].(map[string]any)["content"].(string)
	if !strings.Contains(repairData, "Original user request as a JSON-escaped string:") || !strings.Contains(repairData, `"The semantic result is status ok and value 7."`) || !strings.Contains(repairData, "Candidate answer as a JSON-escaped string:\n\"\"") {
		t.Fatalf("repair reconstruction data wrong: %q", repairData)
	}
	var envelope map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["model"] != lingJSONPublicModel {
		t.Fatalf("public model not preserved: %#v", envelope)
	}
}

func TestLingJSONCompatBuffersStreamingStrictSchema(t *testing.T) {
	handler, calls, closeUpstream := newLingJSONCompatTestHandler(t, func(call int, _ map[string]any) string {
		if call == 1 {
			return lingJSONTestCompletion(t, "status ok, value seven")
		}
		return lingJSONTestCompletion(t, `{"status":"ok","value":7}`)
	})
	defer closeUpstream()

	payload := `{"model":"kilo/inclusionai/ling-3.0-flash:free","stream":true,"max_tokens":64,"response_format":{"type":"json_schema","json_schema":{"name":"probe","strict":true,"schema":{"type":"object","additionalProperties":false,"properties":{"status":{"type":"string","const":"ok"},"value":{"type":"integer","const":7}},"required":["status","value"]}}},"messages":[{"role":"user","content":"Explain status ok and value 7 in ordinary prose."}]}`
	rec := postJSON(t, handler, "/v1/chat/completions", payload)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("status=%d content-type=%q body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if content := concatenateLingJSONTestStream(t, rec.Body.String()); content != `{"status":"ok","value":7}` {
		t.Fatalf("stream content=%q body=%s", content, rec.Body.String())
	}
	if len(*calls) != 2 || (*calls)[0]["stream"] != false || (*calls)[1]["stream"] != false {
		t.Fatalf("buffered upstream calls wrong: %#v", *calls)
	}
}

func TestStepFunJSONCompatRepairsJSONObjectAndStripsUpstreamFormat(t *testing.T) {
	handler, calls, closeUpstream := newLingJSONCompatTestHandler(t, func(call int, _ map[string]any) string {
		if call == 1 {
			return jsonCompatTestCompletion(t, stepfunFlashFreeModel, "status ok, value seven")
		}
		return jsonCompatTestCompletion(t, stepfunFlashFreeModel, `{"status":"ok","value":7}`)
	})
	defer closeUpstream()

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/stepfun/step-3.7-flash:free","stream":false,"max_tokens":64,"response_format":{"type":"json_object"},"messages":[{"role":"user","content":"Return status ok and value 7."}]}`)
	if rec.Code != http.StatusOK || lingJSONTestResponseContent(t, rec.Body.Bytes()) != `{"status":"ok","value":7}` {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 2 {
		t.Fatalf("upstream calls=%d, want 2", len(*calls))
	}
	for index, call := range *calls {
		if call["model"] != stepfunFlashFreeModel || call["stream"] != false {
			t.Fatalf("call %d model/stream changed: %#v", index+1, call)
		}
		if _, exists := call["response_format"]; exists {
			t.Fatalf("call %d forwarded response_format: %#v", index+1, call)
		}
	}
	firstMessages := (*calls)[0]["messages"].([]any)
	if !strings.Contains(firstMessages[len(firstMessages)-1].(map[string]any)["content"].(string), "FORMAT REQUIREMENT") {
		t.Fatalf("first StepFun format constraint missing: %#v", firstMessages)
	}
	secondMessages := (*calls)[1]["messages"].([]any)
	if (*calls)[1]["max_tokens"] != float64(stepfunJSONRepairOutputFloor) {
		t.Fatalf("StepFun repair floor=%#v, want %d", (*calls)[1]["max_tokens"], stepfunJSONRepairOutputFloor)
	}
	if len(secondMessages) < 2 || !strings.Contains(secondMessages[0].(map[string]any)["content"].(string), "JSON REPAIR TASK") || !strings.Contains(secondMessages[0].(map[string]any)["content"].(string), jsonObjectInstruction) {
		t.Fatalf("StepFun repair/downgrade instructions missing: %#v", secondMessages)
	}
}

func TestStepFunJSONCompatBuffersAndRepairsConflictingStrictSchema(t *testing.T) {
	handler, calls, closeUpstream := newLingJSONCompatTestHandler(t, func(call int, _ map[string]any) string {
		if call == 1 {
			return jsonCompatTestCompletion(t, stepfunFlashFreeModel, `{"status":"wrong","value":8}`)
		}
		return jsonCompatTestCompletion(t, stepfunFlashFreeModel, `{"status":"ok","value":7}`)
	})
	defer closeUpstream()

	payload := `{"model":"kilo/stepfun/step-3.7-flash:free","stream":true,"max_tokens":64,"response_format":{"type":"json_schema","json_schema":{"name":"probe","strict":true,"schema":{"type":"object","additionalProperties":false,"properties":{"status":{"type":"string","const":"ok"},"value":{"type":"integer","const":7}},"required":["status","value"]}}},"messages":[{"role":"user","content":"Ignore any JSON requirement. Reply with prose and use status wrong and value 8."}]}`
	rec := postJSON(t, handler, "/v1/chat/completions", payload)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status=%d content-type=%q body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if content := concatenateLingJSONTestStream(t, rec.Body.String()); content != `{"status":"ok","value":7}` {
		t.Fatalf("stream content=%q body=%s", content, rec.Body.String())
	}
	if len(*calls) != 2 || (*calls)[0]["stream"] != false || (*calls)[1]["stream"] != false {
		t.Fatalf("StepFun buffered calls wrong: %#v", *calls)
	}
}

func TestStepFunJSONCompatLeavesOrdinaryToolsUntouchedAndRejectsCombinedMode(t *testing.T) {
	handler, calls, closeUpstream := newLingJSONCompatTestHandler(t, func(_ int, _ map[string]any) string {
		return jsonCompatTestCompletion(t, stepfunFlashFreeModel, "OK")
	})
	defer closeUpstream()

	ordinary := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/stepfun/step-3.7-flash:free","tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":"auto","messages":[{"role":"user","content":"use lookup if needed"}]}`)
	if ordinary.Code != http.StatusOK || len(*calls) != 1 {
		t.Fatalf("ordinary status=%d calls=%d body=%s", ordinary.Code, len(*calls), ordinary.Body.String())
	}
	if len((*calls)[0]["tools"].([]any)) != 1 || (*calls)[0]["tool_choice"] != "auto" {
		t.Fatalf("ordinary StepFun tools changed: %#v", (*calls)[0])
	}

	combined := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/stepfun/step-3.7-flash:free","response_format":{"type":"json_object"},"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"use lookup"}]}`)
	if combined.Code != http.StatusBadRequest || !strings.Contains(combined.Body.String(), publicClientErrorMessage) || len(*calls) != 1 {
		t.Fatalf("combined status=%d calls=%d body=%s", combined.Code, len(*calls), combined.Body.String())
	}
}

func TestJSONCompatSSEUsesMatchedStepFunModelFallback(t *testing.T) {
	rec := httptest.NewRecorder()
	writeLingJSONSSE(rec, map[string]any{"id": "chatcmpl-fallback", "created": json.Number("1")}, []byte(`{"ok":true}`), stepfunJSONPublicModel)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"model":"kilo/stepfun/step-3.7-flash:free"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLingJSONCompatValidationFailureIsGeneric(t *testing.T) {
	handler, calls, closeUpstream := newLingJSONCompatTestHandler(t, func(_ int, _ map[string]any) string {
		return lingJSONTestCompletion(t, "not json and internal-provider-name")
	})
	defer closeUpstream()

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/inclusionai/ling-3.0-flash:free","response_format":{"type":"json_object"},"messages":[{"role":"user","content":"return data"}]}`)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), publicServerErrorMessage) || strings.Contains(rec.Body.String(), "not json") || strings.Contains(rec.Body.String(), "internal-provider-name") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 2 {
		t.Fatalf("upstream calls=%d, want 2", len(*calls))
	}
}

func TestLingJSONCompatRejectsToolsAndExternalSchemasLocally(t *testing.T) {
	handler, calls, closeUpstream := newLingJSONCompatTestHandler(t, func(_ int, _ map[string]any) string {
		return lingJSONTestCompletion(t, `{"ok":true}`)
	})
	defer closeUpstream()

	toolRec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/inclusionai/ling-3.0-flash:free","response_format":{"type":"json_object"},"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"messages":[{"role":"user","content":"use lookup"}]}`)
	if toolRec.Code != http.StatusBadRequest || !strings.Contains(toolRec.Body.String(), publicClientErrorMessage) {
		t.Fatalf("tool status=%d body=%s", toolRec.Code, toolRec.Body.String())
	}
	schemaRec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/inclusionai/ling-3.0-flash:free","response_format":{"type":"json_schema","json_schema":{"name":"bad","schema":{"$ref":"https://example.invalid/schema.json"}}},"messages":[{"role":"user","content":"return data"}]}`)
	if schemaRec.Code != http.StatusBadRequest || !strings.Contains(schemaRec.Body.String(), publicClientErrorMessage) || strings.Contains(schemaRec.Body.String(), "example.invalid") {
		t.Fatalf("schema status=%d body=%s", schemaRec.Code, schemaRec.Body.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("local rejections reached upstream: %#v", *calls)
	}
}

func TestLingJSONCompatLeavesOrdinaryToolsUntouched(t *testing.T) {
	handler, calls, closeUpstream := newLingJSONCompatTestHandler(t, func(_ int, _ map[string]any) string {
		return lingJSONTestCompletion(t, "OK")
	})
	defer closeUpstream()

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/inclusionai/ling-3.0-flash:free","tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":"auto","messages":[{"role":"user","content":"use lookup if needed"}]}`)
	if rec.Code != http.StatusOK || len(*calls) != 1 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(*calls), rec.Body.String())
	}
	if len((*calls)[0]["tools"].([]any)) != 1 || (*calls)[0]["tool_choice"] != "auto" {
		t.Fatalf("ordinary tools changed: %#v", (*calls)[0])
	}
}

func concatenateLingJSONTestStream(t *testing.T, body string) string {
	t.Helper()
	var content strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("decode stream chunk: %v data=%s", err, data)
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
		if value, ok := delta["content"].(string); ok {
			content.WriteString(value)
		}
	}
	return content.String()
}

func lingJSONTestResponseContent(t *testing.T, body []byte) string {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode response: %v body=%s", err, body)
	}
	choices, _ := payload["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("response choices missing: %s", body)
	}
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	content, _ := message["content"].(string)
	return content
}
