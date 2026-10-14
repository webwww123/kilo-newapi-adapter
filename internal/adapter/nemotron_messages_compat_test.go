package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestNemotronSuperHistoricalThinkingPreservesConversation(t *testing.T) {
	body := []byte(`{"model":"kilo/nvidia/nemotron-3-super-120b-a12b:free","max_tokens":512,"messages":[{"role":"user","content":"Check status."},{"role":"assistant","content":[{"type":"thinking","thinking":"Earlier reasoning."},{"type":"thinking","thinking":"Signed reasoning.","signature":"existing-signature"},{"type":"tool_use","id":"call_check","name":"check","input":{"path":"fixture"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_check","content":"ready"},{"type":"text","text":"Summarize this result."}]}],"tools":[{"name":"check","description":"Check status","input_schema":{"type":"object","properties":{"path":{"type":"string"}}}}]}`)
	out, stats, err := normalizeKiloToolHistoryRequest(body, "/messages")
	if err != nil || stats.EmptyThinkingSignatures != 1 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	var before, after map[string]any
	if err := json.Unmarshal(body, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatal(err)
	}
	blocks := before["messages"].([]any)[1].(map[string]any)["content"].([]any)
	blocks[0].(map[string]any)["signature"] = ""
	if !reflect.DeepEqual(before, after) {
		t.Fatal("normalization changed history, existing signatures, tools, or caller limits")
	}
	for _, path := range []string{"/chat/completions", "/responses"} {
		result, unchanged, err := normalizeKiloToolHistoryRequest(body, path)
		if err != nil || !bytes.Equal(body, result) || unchanged != (toolHistoryCompatStats{}) {
			t.Fatalf("unverified protocol %s changed", path)
		}
	}
	var pending map[string]any
	if err := json.Unmarshal(body, &pending); err != nil {
		t.Fatal(err)
	}
	pending["messages"] = pending["messages"].([]any)[:2]
	pendingBody, _ := json.Marshal(pending)
	result, unchanged, err := normalizeKiloToolHistoryRequest(pendingBody, "/messages")
	if err != nil || !bytes.Equal(pendingBody, result) || unchanged != (toolHistoryCompatStats{}) {
		t.Fatal("active assistant thinking was changed")
	}
}

func TestNemotronSuperMessagesHistoryReachesUpstream(t *testing.T) {
	attempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		messages := payload["messages"].([]any)
		thinking := messages[1].(map[string]any)["content"].([]any)[0].(map[string]any)
		if _, ok := thinking["signature"].(string); !ok {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"Invalid Anthropic Messages API request"}}`)
			return
		}
		if r.URL.Path != "/messages" || thinking["thinking"] != "Earlier reasoning." || messages[2].(map[string]any)["content"] != "Reply OK." {
			t.Error("protocol or conversation changed")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"fixture","type":"message","role":"assistant","model":"nvidia/nemotron-3-super-120b-a12b:free","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	handler := NewHandler(Config{UpstreamBaseURL: upstream.URL, RequestLogDir: t.TempDir()})
	body := `{"model":"kilo/nvidia/nemotron-3-super-120b-a12b:free","max_tokens":512,"messages":[{"role":"user","content":"Say hi."},{"role":"assistant","content":[{"type":"thinking","thinking":"Earlier reasoning."},{"type":"text","text":"Hi."}]},{"role":"user","content":"Reply OK."}]}`
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != 200 || attempts != 1 || !strings.Contains(recorder.Body.String(), `"text":"OK"`) {
		t.Fatalf("status=%d attempts=%d; no valid answer", recorder.Code, attempts)
	}
}
