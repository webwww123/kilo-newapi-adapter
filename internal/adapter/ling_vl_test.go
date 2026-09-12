package adapter

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestLingVLJSONRepairValidatesWithoutNativeFormatOrOutputRewrite(t *testing.T) {
	for _, stream := range []bool{false, true} {
		h, calls, closeUpstream := newLingJSONCompatTestHandler(t, func(attempt int, payload map[string]any) string {
			if _, exists := payload["response_format"]; exists {
				t.Fatal("VL upstream rejects native response_format, including repair")
			}
			if payload["max_tokens"] != float64(128) {
				t.Fatal("caller output budget changed")
			}
			if attempt == 1 {
				return jsonCompatTestCompletion(t, lingVLFreeModel, `{"ok":false}`)
			}
			return jsonCompatTestCompletion(t, lingVLFreeModel, `{"ok":true}`)
		})
		body := mustJSON(t, map[string]any{
			"model": lingVLJSONPublicModel, "stream": stream, "max_tokens": 128,
			"messages":        []any{map[string]any{"role": "user", "content": "Return false in plain prose."}},
			"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "answer", "strict": true, "schema": map[string]any{"type": "object", "properties": map[string]any{"ok": map[string]any{"const": true}}, "required": []string{"ok"}, "additionalProperties": false}}},
		})
		rec := postJSON(t, h, "/v1/chat/completions", string(body))
		closeUpstream()
		if rec.Code != http.StatusOK || len(*calls) != 2 {
			t.Fatalf("status=%d attempts=%d", rec.Code, len(*calls))
		}
		if !stream && lingJSONTestResponseContent(t, rec.Body.Bytes()) != `{"ok":true}` {
			t.Fatal("unvalidated JSON returned")
		}
		if stream && (!strings.Contains(rec.Body.String(), "[DONE]") || !strings.Contains(rec.Body.String(), `\"ok\":true`)) {
			t.Fatal("stream omitted validated JSON or terminal marker")
		}
	}
}

func TestLingVLLeavesVisionAndOrdinaryToolsIntact(t *testing.T) {
	h, calls, closeUpstream := newLingJSONCompatTestHandler(t, func(_ int, _ map[string]any) string {
		return jsonCompatTestCompletion(t, lingVLFreeModel, "OK")
	})
	defer closeUpstream()
	body := `{"model":"kilo/inclusionai/ling-3.0-flash-vl:free","max_tokens":64,"messages":[{"role":"user","content":[{"type":"text","text":"Inspect image"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}],"tools":[{"type":"function","function":{"name":"inspect","parameters":{"type":"object"}}}],"tool_choice":"auto"}`
	rec := postJSON(t, h, "/v1/chat/completions", body)
	if rec.Code != http.StatusOK || len(*calls) != 1 {
		t.Fatalf("ordinary vision/tool status=%d", rec.Code)
	}
	forwarded := mustJSON(t, (*calls)[0])
	for _, marker := range []string{"image_url", "data:image/png;base64,AA", "inspect", "tool_choice"} {
		if !strings.Contains(string(forwarded), marker) {
			t.Fatalf("lost %s", marker)
		}
	}
	if (*calls)[0]["max_tokens"] != float64(64) {
		t.Fatal("ordinary output limit changed")
	}
}

func TestLingVLJSONRejectsInvalidOutputAfterOneRepair(t *testing.T) {
	h, calls, closeUpstream := newLingJSONCompatTestHandler(t, func(_ int, _ map[string]any) string {
		return jsonCompatTestCompletion(t, lingVLFreeModel, "not JSON")
	})
	defer closeUpstream()
	rec := postJSON(t, h, "/v1/chat/completions", `{"model":"kilo/inclusionai/ling-3.0-flash-vl:free","messages":[{"role":"user","content":"Return JSON"}],"response_format":{"type":"json_object"},"max_tokens":512}`)
	if rec.Code < 400 || len(*calls) != 2 {
		t.Fatalf("invalid output escaped validation: status=%d calls=%d", rec.Code, len(*calls))
	}
}

func TestLingVLContextKeepsToolPairAndLatestImage(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"model": lingVLJSONPublicModel, "max_tokens": 2048,
		"messages": []any{
			map[string]any{"role": "system", "content": "KEEP_SYSTEM"},
			map[string]any{"role": "user", "content": "DROP_OLD" + strings.Repeat("old ", 400000)},
			map[string]any{"role": "assistant", "content": "Old answer"},
			map[string]any{"role": "user", "content": "KEEP_TASK"},
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_keep", "type": "function", "function": map[string]any{"name": "read", "arguments": `{}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "call_keep", "content": "KEEP_RESULT"},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "KEEP_LATEST"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AA"}}}},
		},
	})
	out, stats, err := trimFreeModelContext(body, "/chat/completions")
	if err != nil || !stats.changed() || strings.Contains(string(out), "DROP_OLD") {
		t.Fatalf("old context not removed: stats=%+v error=%v", stats, err)
	}
	for _, marker := range []string{"KEEP_SYSTEM", "KEEP_TASK", "KEEP_RESULT", "KEEP_LATEST", "call_keep", "data:image/png;base64,AA"} {
		if !strings.Contains(string(out), marker) {
			t.Fatalf("lost %s", marker)
		}
	}
	var payload map[string]any
	if json.Unmarshal(out, &payload) != nil || payload["max_tokens"] != float64(2048) {
		t.Fatal("output budget or JSON corrupted")
	}
	if _, ok := freeContextTrimProfiles["inclusionai/ling-3.0-flash-vl"]; ok {
		t.Fatal("paid VL must remain unchanged")
	}
}
