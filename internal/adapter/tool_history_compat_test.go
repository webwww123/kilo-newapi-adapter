package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func toolHistoryFixture(arguments string, completed bool) []byte {
	field := ""
	if arguments != "" {
		field = `,"arguments":` + arguments
	}
	result := ""
	if completed {
		result = `,{"role":"tool","tool_call_id":"call_fixture","content":"completed"},{"role":"user","content":"Summarize the result."}`
	}
	return []byte(fmt.Sprintf(`{"model":"kilo/stepfun/step-3.7-flash:free","metadata":{"large":9007199254740993},"messages":[{"role":"user","content":"Check status."},{"role":"assistant","content":null,"tool_calls":[{"id":"call_fixture","type":"function","function":{"name":"check"%s}}]}%s]}`, field, result))
}

func TestKiloCompletedToolHistoryArguments(t *testing.T) {
	for _, tt := range []struct {
		name          string
		input         string
		want          string
		empty, object int
	}{
		{"missing", "", "", 1, 0},
		{"null", "null", "", 1, 0},
		{"object", `{"id":9007199254740993,"path":"C:\\work"}`, `{"id":9007199254740993,"path":"C:\\work"}`, 0, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out, stats, err := normalizeKiloToolHistoryRequest(toolHistoryFixture(tt.input, true), "/chat/completions")
			if err != nil || stats.EmptyArguments != tt.empty || stats.ObjectArguments != tt.object {
				t.Fatalf("stats=%+v err=%v", stats, err)
			}
			var payload struct {
				Metadata map[string]json.RawMessage `json:"metadata"`
				Messages []struct {
					ToolCalls []struct {
						Function map[string]json.RawMessage `json:"function"`
					} `json:"tool_calls"`
					Content json.RawMessage `json:"content"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(out, &payload); err != nil {
				t.Fatal(err)
			}
			arguments, exists := payload.Messages[1].ToolCalls[0].Function["arguments"]
			if !exists || toolHistoryString(arguments) != tt.want {
				t.Fatalf("wrong argument representation: %s", arguments)
			}
			if string(payload.Metadata["large"]) != "9007199254740993" || string(payload.Messages[1].Content) != "null" || len(payload.Messages) != 4 {
				t.Fatal("unrelated request information changed")
			}
			again, againStats, err := normalizeKiloToolHistoryRequest(out, "/chat/completions")
			if err != nil || !bytes.Equal(again, out) || againStats != (toolHistoryCompatStats{}) {
				t.Fatal("normalization is not idempotent")
			}
		})
	}
}

func TestKiloToolHistoryLeavesUnsupportedOrAmbiguousInputsUntouched(t *testing.T) {
	for name, body := range map[string][]byte{
		"pending":           toolHistoryFixture("", false),
		"valid_string":      toolHistoryFixture(`"{\"name\":\"sample\"}"`, true),
		"invalid_string":    toolHistoryFixture(`"{broken"`, true),
		"array":             toolHistoryFixture(`[]`, true),
		"other_model":       bytes.ReplaceAll(toolHistoryFixture("", true), []byte("stepfun/step-3.7-flash:free"), []byte("unrelated/model")),
		"mismatched_result": bytes.Replace(toolHistoryFixture("", true), []byte(`"tool_call_id":"call_fixture"`), []byte(`"tool_call_id":"other"`), 1),
		"duplicate_result":  bytes.Replace(toolHistoryFixture("", true), []byte(`{"role":"user","content":"Summarize the result."}`), []byte(`{"role":"tool","tool_call_id":"call_fixture","content":"duplicate"}`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			out, stats, err := normalizeKiloToolHistoryRequest(body, "/chat/completions")
			if err != nil || !bytes.Equal(body, out) || stats != (toolHistoryCompatStats{}) {
				t.Fatalf("unexpected mutation: stats=%+v err=%v", stats, err)
			}
		})
	}
	body := toolHistoryFixture("", true)
	out, _, err := normalizeKiloToolHistoryRequest(body, "/messages")
	if err != nil || !bytes.Equal(body, out) {
		t.Fatal("wrong protocol was changed")
	}
}

func TestKiloCompletedToolHistoryReachesUpstreamWithStringArguments(t *testing.T) {
	attempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var payload struct {
			Messages []struct {
				ToolCalls []struct {
					Function map[string]json.RawMessage `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		raw, exists := payload.Messages[1].ToolCalls[0].Function["arguments"]
		var arguments string
		if !exists || json.Unmarshal(raw, &arguments) != nil {
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"test","object":"chat.completion","model":"stepfun/step-3.7-flash:free","choices":[{"index":0,"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	handler := NewHandler(Config{UpstreamBaseURL: upstream.URL, UpstreamAPIKey: "test-only", RequestLogDir: t.TempDir()})
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(toolHistoryFixture("", true)))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(recorder, req)
	if recorder.Code != 200 || attempts != 1 || !strings.Contains(recorder.Body.String(), `"content":"OK"`) {
		t.Fatalf("status=%d attempts=%d", recorder.Code, attempts)
	}
}

func TestKiloInklingHistoricalThinkingMissingSignature(t *testing.T) {
	body := []byte(`{"model":"kilo/thinkingmachines/inkling:free","messages":[{"role":"user","content":"Check status."},{"role":"assistant","content":[{"type":"thinking","thinking":"Earlier status was checked."},{"type":"thinking","thinking":"Signed history.","signature":"opaque-existing"},{"type":"text","text":"OK"}]},{"role":"user","content":"Continue."}]}`)
	out, stats, err := normalizeKiloToolHistoryRequest(body, "/messages")
	if err != nil || stats.EmptyThinkingSignatures != 1 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	if !bytes.Contains(out, []byte(`"signature":""`)) || !bytes.Contains(out, []byte(`"signature":"opaque-existing"`)) || !bytes.Contains(out, []byte(`Earlier status was checked.`)) {
		t.Fatal("signature placeholder missing or historical information changed")
	}
	for _, variant := range [][]byte{
		bytes.ReplaceAll(body, []byte("thinkingmachines/inkling:free"), []byte("stepfun/step-3.7-flash:free")),
		bytes.Replace(body, []byte(`,{"role":"user","content":"Continue."}`), nil, 1),
	} {
		result, unchanged, err := normalizeKiloToolHistoryRequest(variant, "/messages")
		if err != nil || !bytes.Equal(result, variant) || unchanged != (toolHistoryCompatStats{}) {
			t.Fatal("unrelated model or active thinking changed")
		}
	}
	again, unchanged, err := normalizeKiloToolHistoryRequest(out, "/messages")
	if err != nil || !bytes.Equal(again, out) || unchanged != (toolHistoryCompatStats{}) {
		t.Fatal("signature normalization is not idempotent")
	}
}

func TestKiloOnboardedFreeModelsGetHistoricalThinkingSignature(t *testing.T) {
	models := []string{
		"kilo/nex-agi/nex-n2.5-pro:free",
		"kilo/nex-agi/nex-n2.5-mini:free",
		"kilo/nvidia/nemotron-3.5-lightning:free",
		"kilo/inclusionai/ling-3.0-flash-fin:free",
		"kilo/inclusionai/ling-3.0-flash-sante:free",
		"kilo/inclusionai/ling-3.0-flash-vl:free",
	}
	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"Check status."},{"role":"assistant","content":[{"type":"thinking","thinking":"Earlier status was checked."},{"type":"text","text":"OK"}]},{"role":"user","content":"Continue."}]}`)
			out, stats, err := normalizeKiloToolHistoryRequest(body, "/messages")
			if err != nil || stats.EmptyThinkingSignatures != 1 {
				t.Fatalf("stats=%+v err=%v", stats, err)
			}
			if !bytes.Contains(out, []byte(`"signature":""`)) || !bytes.Contains(out, []byte(`Earlier status was checked.`)) {
				t.Fatalf("signature placeholder missing or history changed: %s", out)
			}
			again, unchanged, err := normalizeKiloToolHistoryRequest(out, "/messages")
			if err != nil || !bytes.Equal(again, out) || unchanged != (toolHistoryCompatStats{}) {
				t.Fatal("signature normalization is not idempotent")
			}
		})
	}

	unlisted := []byte(`{"model":"kilo/openrouter/free","messages":[{"role":"user","content":"Check status."},{"role":"assistant","content":[{"type":"thinking","thinking":"Earlier status was checked."}]},{"role":"user","content":"Continue."}]}`)
	if out, stats, err := normalizeKiloToolHistoryRequest(unlisted, "/messages"); err != nil || !bytes.Equal(out, unlisted) || stats != (toolHistoryCompatStats{}) {
		t.Fatal("unlisted model history was rewritten")
	}
}
