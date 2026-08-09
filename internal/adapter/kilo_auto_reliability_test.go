package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newKiloAutoReliabilityTestHandler(t *testing.T, responder func(int, map[string]any, http.ResponseWriter)) (http.Handler, *[]map[string]any, func()) {
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
		responder(len(calls), payload, w)
	}))
	cfg := Config{
		UpstreamBaseURL:            upstream.URL,
		HTTPClient:                 upstream.Client(),
		RequestLogDir:              t.TempDir(),
		KiloAutoReliabilityEnabled: true,
	}
	return NewHandler(cfg), &calls, upstream.Close
}

func TestKiloAutoReliabilityIsDisabledByDefault(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		writeKiloAutoCompletion(t, w, "", "PRIVATE_DEFAULT_OFF", "length", nil)
	}))
	defer upstream.Close()
	handler := NewHandler(Config{UpstreamBaseURL: upstream.URL, HTTPClient: upstream.Client(), RequestLogDir: t.TempDir()})

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK || calls != 1 || !strings.Contains(rec.Body.String(), "PRIVATE_DEFAULT_OFF") {
		t.Fatalf("default-off behavior changed: status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
	}
}

func TestKiloAutoReliabilityEnvironmentFlag(t *testing.T) {
	t.Setenv("KILO_AUTO_RELIABILITY_ENABLED", "true")
	if cfg := ConfigFromEnv(); !cfg.KiloAutoReliabilityEnabled {
		t.Fatal("KILO_AUTO_RELIABILITY_ENABLED=true was not parsed")
	}
	t.Setenv("KILO_AUTO_RELIABILITY_ENABLED", "false")
	if cfg := ConfigFromEnv(); cfg.KiloAutoReliabilityEnabled {
		t.Fatal("KILO_AUTO_RELIABILITY_ENABLED=false was not parsed")
	}
}

func writeKiloAutoCompletion(t *testing.T, w http.ResponseWriter, content, reasoning, finish string, toolCalls []any) {
	t.Helper()
	message := map[string]any{
		"role":    "assistant",
		"content": content,
	}
	if reasoning != "" {
		message["reasoning"] = reasoning
		message["reasoning_details"] = []any{map[string]any{"type": "reasoning.text", "text": reasoning}}
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	body, err := json.Marshal(map[string]any{
		"id":      "chatcmpl-auto-reliability",
		"object":  "chat.completion",
		"created": 1,
		"model":   kiloAutoFreeModel,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func kiloAutoReasoningMaxTokens(t *testing.T, payload map[string]any) int64 {
	t.Helper()
	reasoning, ok := payload["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("reasoning missing or invalid: %#v", payload["reasoning"])
	}
	value, ok := lingJSONInteger(reasoning["max_tokens"])
	if !ok {
		t.Fatalf("reasoning.max_tokens invalid: %#v", reasoning)
	}
	return value
}

func kiloAutoMessagesContain(payload map[string]any, needle string) bool {
	messages, _ := payload["messages"].([]any)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if strings.Contains(fmt.Sprint(message["content"]), needle) {
			return true
		}
	}
	return false
}

func TestKiloAutoReliabilityRepairsEmptyNonStream(t *testing.T) {
	const privateReasoning = "PRIVATE_FIRST_REASONING"
	handler, calls, closeUpstream := newKiloAutoReliabilityTestHandler(t, func(call int, _ map[string]any, w http.ResponseWriter) {
		if call == 1 {
			writeKiloAutoCompletion(t, w, "", privateReasoning, "length", nil)
			return
		}
		writeKiloAutoCompletion(t, w, "COMPLETE", "", "stop", nil)
	})
	defer closeUpstream()

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","max_tokens":64,"messages":[{"role":"user","content":"write code"}]}`)
	if rec.Code != http.StatusOK || lingJSONTestResponseContent(t, rec.Body.Bytes()) != "COMPLETE" {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 2 {
		t.Fatalf("calls=%d, want 2: %#v", len(*calls), *calls)
	}
	if got := kiloAutoReasoningMaxTokens(t, (*calls)[0]); got != 512 {
		t.Fatalf("first reasoning cap=%d, want 512", got)
	}
	if got := kiloAutoReasoningMaxTokens(t, (*calls)[1]); got != 256 {
		t.Fatalf("repair reasoning cap=%d, want 256", got)
	}
	if got, _ := lingJSONInteger((*calls)[0]["max_tokens"]); got != 64 {
		t.Fatalf("first max_tokens=%d, want 64", got)
	}
	if got, _ := lingJSONInteger((*calls)[1]["max_tokens"]); got != 10000 {
		t.Fatalf("repair max_tokens=%d, want 10000", got)
	}
	if !kiloAutoMessagesContain((*calls)[1], "complete visible answer") {
		t.Fatalf("repair instruction missing: %#v", (*calls)[1]["messages"])
	}
	if strings.Contains(rec.Body.String(), privateReasoning) {
		t.Fatalf("discarded reasoning leaked: %s", rec.Body.String())
	}
}

func TestKiloAutoReliabilityRepairsVisibleLengthFinish(t *testing.T) {
	handler, calls, closeUpstream := newKiloAutoReliabilityTestHandler(t, func(call int, _ map[string]any, w http.ResponseWriter) {
		if call == 1 {
			writeKiloAutoCompletion(t, w, "partial answer", "", "length", nil)
			return
		}
		writeKiloAutoCompletion(t, w, "complete answer", "", "stop", nil)
	})
	defer closeUpstream()

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"write code"}]}`)
	if rec.Code != http.StatusOK || lingJSONTestResponseContent(t, rec.Body.Bytes()) != "complete answer" || len(*calls) != 2 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(*calls), rec.Body.String())
	}
}

func TestKiloAutoReliabilityLeavesUsefulToolCallUnchanged(t *testing.T) {
	toolCalls := []any{map[string]any{
		"id":   "call_1",
		"type": "function",
		"function": map[string]any{
			"name":      "lookup",
			"arguments": `{"q":"x"}`,
		},
	}}
	handler, calls, closeUpstream := newKiloAutoReliabilityTestHandler(t, func(_ int, _ map[string]any, w http.ResponseWriter) {
		writeKiloAutoCompletion(t, w, "", "", "tool_calls", toolCalls)
	})
	defer closeUpstream()

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":"auto","messages":[{"role":"user","content":"use lookup"}]}`)
	if rec.Code != http.StatusOK || len(*calls) != 1 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(*calls), rec.Body.String())
	}
	if _, exists := (*calls)[0]["reasoning"]; exists {
		t.Fatalf("successful tool request gained reasoning controls: %#v", (*calls)[0])
	}
	if !strings.Contains(rec.Body.String(), `"tool_calls"`) {
		t.Fatalf("tool call missing: %s", rec.Body.String())
	}
}

func TestKiloAutoReliabilityRepairsEmptyToolRequestAndPreservesTools(t *testing.T) {
	toolCalls := []any{map[string]any{
		"id":   "call_2",
		"type": "function",
		"function": map[string]any{
			"name":      "lookup",
			"arguments": `{"q":"fixed"}`,
		},
	}}
	handler, calls, closeUpstream := newKiloAutoReliabilityTestHandler(t, func(call int, _ map[string]any, w http.ResponseWriter) {
		if call == 1 {
			writeKiloAutoCompletion(t, w, "", "PRIVATE_TOOL_REASONING", "length", nil)
			return
		}
		writeKiloAutoCompletion(t, w, "", "", "tool_calls", toolCalls)
	})
	defer closeUpstream()

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","max_tokens":64,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":"auto","messages":[{"role":"user","content":"use lookup"}]}`)
	if rec.Code != http.StatusOK || len(*calls) != 2 || !strings.Contains(rec.Body.String(), `"tool_calls"`) {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(*calls), rec.Body.String())
	}
	if _, exists := (*calls)[0]["reasoning"]; exists {
		t.Fatalf("first tool attempt gained reasoning controls: %#v", (*calls)[0])
	}
	if got := kiloAutoReasoningMaxTokens(t, (*calls)[1]); got != 256 {
		t.Fatalf("repair reasoning cap=%d, want 256", got)
	}
	if (*calls)[1]["tool_choice"] != "auto" || len((*calls)[1]["tools"].([]any)) != 1 {
		t.Fatalf("repair lost tools: %#v", (*calls)[1])
	}
}

func TestKiloAutoReliabilityRespectsExplicitFirstReasoningControls(t *testing.T) {
	tests := []struct {
		name       string
		reasoning  string
		wantValue  int64
		wantScalar any
	}{
		{name: "preserve lower cap", reasoning: `{"max_tokens":128}`, wantValue: 128},
		{name: "clamp higher cap", reasoning: `{"max_tokens":2048}`, wantValue: 512},
		{name: "preserve disabled", reasoning: `false`, wantScalar: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler, calls, closeUpstream := newKiloAutoReliabilityTestHandler(t, func(_ int, _ map[string]any, w http.ResponseWriter) {
				writeKiloAutoCompletion(t, w, "OK", "", "stop", nil)
			})
			defer closeUpstream()

			body := `{"model":"kilo/kilo-auto/free","reasoning":` + tc.reasoning + `,"messages":[{"role":"user","content":"hello"}]}`
			rec := postJSON(t, handler, "/v1/chat/completions", body)
			if rec.Code != http.StatusOK || len(*calls) != 1 {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(*calls), rec.Body.String())
			}
			if tc.wantScalar != nil {
				if (*calls)[0]["reasoning"] != tc.wantScalar {
					t.Fatalf("reasoning=%#v, want %#v", (*calls)[0]["reasoning"], tc.wantScalar)
				}
				return
			}
			if got := kiloAutoReasoningMaxTokens(t, (*calls)[0]); got != tc.wantValue {
				t.Fatalf("reasoning cap=%d, want %d", got, tc.wantValue)
			}
		})
	}
}

func TestKiloAutoReliabilityLeavesStructuredJSONToExistingHandler(t *testing.T) {
	handler, calls, closeUpstream := newKiloAutoReliabilityTestHandler(t, func(_ int, payload map[string]any, w http.ResponseWriter) {
		writeKiloAutoCompletion(t, w, `{"ok":true}`, "", "stop", nil)
	})
	defer closeUpstream()

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","max_tokens":1024,"response_format":{"type":"json_object"},"messages":[{"role":"user","content":"return JSON"}]}`)
	if rec.Code != http.StatusOK || len(*calls) != 1 || lingJSONTestResponseContent(t, rec.Body.Bytes()) != `{"ok":true}` {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(*calls), rec.Body.String())
	}
	if _, exists := (*calls)[0]["reasoning"]; exists {
		t.Fatalf("structured JSON request was modified by ordinary reliability wrapper: %#v", (*calls)[0])
	}
	if _, exists := (*calls)[0]["response_format"]; exists {
		t.Fatalf("existing JSON handler did not consume response_format: %#v", (*calls)[0])
	}
}

func TestKiloAutoReliabilityDoesNotRetryPolicyFinish(t *testing.T) {
	handler, calls, closeUpstream := newKiloAutoReliabilityTestHandler(t, func(_ int, _ map[string]any, w http.ResponseWriter) {
		writeKiloAutoCompletion(t, w, "", "", "content_filter", nil)
	})
	defer closeUpstream()

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK || len(*calls) != 1 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(*calls), rec.Body.String())
	}
}

func writeKiloAutoSSE(w http.ResponseWriter, content, reasoning, finish string) {
	w.Header().Set("Content-Type", "text/event-stream")
	first := map[string]any{
		"id":      "chatcmpl-auto-stream",
		"object":  "chat.completion.chunk",
		"created": 1,
		"model":   kiloAutoFreeModel,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"role":      "assistant",
				"content":   content,
				"reasoning": reasoning,
			},
			"finish_reason": nil,
		}},
	}
	final := map[string]any{
		"id":      "chatcmpl-auto-stream",
		"object":  "chat.completion.chunk",
		"created": 1,
		"model":   kiloAutoFreeModel,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finish,
		}},
	}
	firstBody, _ := json.Marshal(first)
	finalBody, _ := json.Marshal(final)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", firstBody)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", finalBody)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
}

func TestKiloAutoReliabilityRetriesEmptyStreamBeforeCommit(t *testing.T) {
	const privateReasoning = "PRIVATE_EMPTY_STREAM_REASONING"
	handler, calls, closeUpstream := newKiloAutoReliabilityTestHandler(t, func(call int, _ map[string]any, w http.ResponseWriter) {
		if call == 1 {
			writeKiloAutoSSE(w, "", privateReasoning, "length")
			return
		}
		writeKiloAutoSSE(w, "OK", "", "stop")
	})
	defer closeUpstream()

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","stream":true,"messages":[{"role":"user","content":"write code"}]}`)
	if rec.Code != http.StatusOK || len(*calls) != 2 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(*calls), rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), privateReasoning) || !strings.Contains(rec.Body.String(), `"content":"OK"`) {
		t.Fatalf("first stream leaked or repaired stream missing: %s", rec.Body.String())
	}
	if got := kiloAutoReasoningMaxTokens(t, (*calls)[1]); got != 256 {
		t.Fatalf("stream repair reasoning cap=%d, want 256", got)
	}
	if got, _ := lingJSONInteger((*calls)[1]["max_tokens"]); got != 10000 {
		t.Fatalf("stream repair max_tokens=%d, want 10000", got)
	}
}

func TestKiloAutoReliabilityDoesNotRetryAfterMeaningfulStream(t *testing.T) {
	handler, calls, closeUpstream := newKiloAutoReliabilityTestHandler(t, func(_ int, _ map[string]any, w http.ResponseWriter) {
		writeKiloAutoSSE(w, "partial", "", "length")
	})
	defer closeUpstream()

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","stream":true,"messages":[{"role":"user","content":"write code"}]}`)
	if rec.Code != http.StatusOK || len(*calls) != 1 || !strings.Contains(rec.Body.String(), `"content":"partial"`) {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(*calls), rec.Body.String())
	}
}

func TestKiloAutoReliabilityReturnsGenericErrorAfterSecondEmptyStream(t *testing.T) {
	handler, calls, closeUpstream := newKiloAutoReliabilityTestHandler(t, func(call int, _ map[string]any, w http.ResponseWriter) {
		writeKiloAutoSSE(w, "", fmt.Sprintf("PRIVATE_STREAM_REASONING_%d", call), "length")
	})
	defer closeUpstream()

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","stream":true,"messages":[{"role":"user","content":"write code"}]}`)
	if rec.Code != http.StatusInternalServerError || len(*calls) != 2 || !strings.Contains(rec.Body.String(), publicServerErrorMessage) {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(*calls), rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "PRIVATE_STREAM_REASONING") {
		t.Fatalf("empty streams leaked: %s", rec.Body.String())
	}
}

func TestKiloAutoStreamPreludeDetectsSplitMeaningfulDelta(t *testing.T) {
	rec := httptest.NewRecorder()
	gate := newKiloAutoStreamPreludeWriter(rec, 4096)
	gate.Header().Set("Content-Type", "text/event-stream")
	gate.WriteHeader(http.StatusOK)
	line := `data: {"choices":[{"delta":{"content":"OK"},"finish_reason":null}]}` + "\n\n"
	middle := len(line) / 2
	if _, err := gate.Write([]byte(line[:middle])); err != nil {
		t.Fatal(err)
	}
	if gate.committed {
		t.Fatal("split prelude committed before the meaningful line completed")
	}
	if _, err := gate.Write([]byte(line[middle:])); err != nil {
		t.Fatal(err)
	}
	if !gate.committed || !gate.meaningful || !strings.Contains(rec.Body.String(), `"content":"OK"`) {
		t.Fatalf("split meaningful delta was not committed: committed=%t meaningful=%t body=%s", gate.committed, gate.meaningful, rec.Body.String())
	}
}

func TestKiloAutoStreamPreludeForcesBoundedCommit(t *testing.T) {
	rec := httptest.NewRecorder()
	gate := newKiloAutoStreamPreludeWriter(rec, 32)
	gate.Header().Set("Content-Type", "text/event-stream")
	gate.WriteHeader(http.StatusOK)
	line := `data: {"choices":[{"delta":{"reasoning":"PRIVATE_LONG_REASONING"},"finish_reason":null}]}` + "\n\n"
	if _, err := gate.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	if !gate.committed || !gate.forcedCommit || gate.meaningful {
		t.Fatalf("bounded prelude did not force commit: committed=%t forced=%t meaningful=%t", gate.committed, gate.forcedCommit, gate.meaningful)
	}
}

func TestKiloAutoReliabilityReturnsGenericErrorAfterSecondEmptyAnswer(t *testing.T) {
	handler, calls, closeUpstream := newKiloAutoReliabilityTestHandler(t, func(call int, _ map[string]any, w http.ResponseWriter) {
		writeKiloAutoCompletion(t, w, "", fmt.Sprintf("PRIVATE_REASONING_%d", call), "length", nil)
	})
	defer closeUpstream()

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"write code"}]}`)
	if rec.Code != http.StatusInternalServerError || len(*calls) != 2 || !strings.Contains(rec.Body.String(), publicServerErrorMessage) {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(*calls), rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "PRIVATE_REASONING") {
		t.Fatalf("failed attempts leaked: %s", rec.Body.String())
	}
}

func TestKiloAutoRepairPreservesSystemAndDeveloperPolicyOrder(t *testing.T) {
	handler, calls, closeUpstream := newKiloAutoReliabilityTestHandler(t, func(call int, _ map[string]any, w http.ResponseWriter) {
		if call == 1 {
			writeKiloAutoCompletion(t, w, "", "PRIVATE_EMPTY", "length", nil)
			return
		}
		writeKiloAutoCompletion(t, w, "OK", "", "stop", nil)
	})
	defer closeUpstream()

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"system","content":"SYSTEM_POLICY"},{"role":"developer","content":"DEVELOPER_POLICY"},{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK || len(*calls) != 2 {
		t.Fatalf("status=%d calls=%d body=%s", rec.Code, len(*calls), rec.Body.String())
	}
	messages := (*calls)[1]["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("repair messages=%#v", messages)
	}
	wantRoles := []string{"system", "system", "developer", "user"}
	for i, want := range wantRoles {
		message := messages[i].(map[string]any)
		if message["role"] != want {
			t.Fatalf("message %d role=%v, want %s: %#v", i, message["role"], want, messages)
		}
	}
	if !strings.Contains(fmt.Sprint(messages[0].(map[string]any)["content"]), "SYSTEM_POLICY") ||
		!strings.Contains(fmt.Sprint(messages[2].(map[string]any)["content"]), "DEVELOPER_POLICY") {
		t.Fatalf("policy messages changed: %#v", messages)
	}
}

func TestKiloAutoRetriesRawTransientStatuses(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			calls := 0
			var bodies [][]byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				bodies = append(bodies, append([]byte(nil), raw...))
				calls++
				if calls <= 2 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"error":{"message":"PRIVATE_TRANSIENT"}}`)
					return
				}
				writeKiloAutoCompletion(t, w, "OK", "", "stop", nil)
			}))
			defer upstream.Close()

			var waits []time.Duration
			handler := newRetryTestHandler(t, upstream.URL, func(ctx context.Context, delay time.Duration) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				waits = append(waits, delay)
				return nil
			})
			rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"hi"}]}`)
			if rec.Code != http.StatusOK || calls != 3 {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body.String())
			}
			wantWaits := []time.Duration{time.Second, 3 * time.Second}
			if len(waits) != len(wantWaits) {
				t.Fatalf("waits=%v, want %v", waits, wantWaits)
			}
			for i := range wantWaits {
				if waits[i] != wantWaits[i] {
					t.Fatalf("wait %d=%v, want %v", i+1, waits[i], wantWaits[i])
				}
			}
			for i := 1; i < len(bodies); i++ {
				if !bytes.Equal(bodies[0], bodies[i]) {
					t.Fatalf("retry body %d differs from first", i+1)
				}
			}
		})
	}
}

func TestKiloAutoRetryExhaustionPreservesSanitizedRateLimit(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 3 {
			w.Header().Set("Retry-After", "17")
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"PRIVATE_RATE_LIMIT","provider":"PRIVATE_PROVIDER"}}`)
	}))
	defer upstream.Close()

	var waits []time.Duration
	handler := newRetryTestHandler(t, upstream.URL, func(context.Context, time.Duration) error {
		waits = append(waits, time.Second)
		return nil
	})
	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusTooManyRequests || calls != 3 || len(waits) != 2 {
		t.Fatalf("status=%d calls=%d waits=%d body=%s", rec.Code, calls, len(waits), rec.Body.String())
	}
	if rec.Header().Get("Retry-After") != "17" || !strings.Contains(rec.Body.String(), publicRateLimitErrorMessage) {
		t.Fatalf("rate-limit response wrong: headers=%v body=%s", rec.Header(), rec.Body.String())
	}
	for _, marker := range []string{"PRIVATE_RATE_LIMIT", "PRIVATE_PROVIDER"} {
		if strings.Contains(rec.Body.String(), marker) {
			t.Fatalf("rate-limit response leaked %q: %s", marker, rec.Body.String())
		}
	}
}

func TestKiloAutoRetryAfterAndBudget(t *testing.T) {
	now := time.Date(2026, time.August, 9, 8, 0, 0, 0, time.UTC)
	zeroJitter := func(time.Duration) time.Duration { return 0 }
	if got, ok := kiloAutoRetryDelay(0, "12", now, 0, zeroJitter); !ok || got != 12*time.Second {
		t.Fatalf("Retry-After delay=%v, %t; want 12s, true", got, ok)
	}
	if got, ok := kiloAutoRetryDelay(1, "", now, 14*time.Second, zeroJitter); !ok || got != time.Second {
		t.Fatalf("budget-clamped delay=%v, %t; want 1s, true", got, ok)
	}
	if _, ok := kiloAutoRetryDelay(1, "", now, 15*time.Second, zeroJitter); ok {
		t.Fatal("retry allowed after Auto wait budget exhausted")
	}
}

func TestKiloAutoTransientRetryIsChatOnly(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()
	waits := 0
	handler := newRetryTestHandler(t, upstream.URL, func(context.Context, time.Duration) error {
		waits++
		return nil
	})
	rec := postJSON(t, handler, "/v1/messages", `{"model":"kilo/kilo-auto/free","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusInternalServerError || calls != 1 || waits != 0 {
		t.Fatalf("status=%d calls=%d waits=%d body=%s", rec.Code, calls, waits, rec.Body.String())
	}
}

func TestKiloAutoDoesNotRetryDeterministicStatuses(t *testing.T) {
	tests := []struct {
		upstream int
		public   int
	}{
		{upstream: http.StatusBadRequest, public: http.StatusBadRequest},
		{upstream: http.StatusRequestEntityTooLarge, public: http.StatusBadRequest},
		{upstream: http.StatusUnavailableForLegalReasons, public: publicReviewStatus},
		{upstream: http.StatusInternalServerError, public: http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(strconv.Itoa(tc.upstream), func(t *testing.T) {
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.upstream)
				_, _ = io.WriteString(w, `{"error":{"message":"PRIVATE_DETERMINISTIC"}}`)
			}))
			defer upstream.Close()

			waits := 0
			handler := newRetryTestHandler(t, upstream.URL, func(context.Context, time.Duration) error {
				waits++
				return nil
			})
			rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"hi"}]}`)
			if rec.Code != tc.public || calls != 1 || waits != 0 {
				t.Fatalf("status=%d calls=%d waits=%d body=%s", rec.Code, calls, waits, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "PRIVATE_DETERMINISTIC") {
				t.Fatalf("private error leaked: %s", rec.Body.String())
			}
		})
	}
}

type kiloAutoRoundTripFunc func(*http.Request) (*http.Response, error)

func (f kiloAutoRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestKiloAutoRetriesPreHeaderTransportFailure(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: kiloAutoRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("PRIVATE_DIAL_FAILURE")
		}
		body := jsonCompatTestCompletion(t, kiloAutoFreeModel, "OK")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    r,
		}, nil
	})}
	base := newBaseHandler(Config{UpstreamBaseURL: "http://upstream.invalid", HTTPClient: client, RequestLogDir: t.TempDir(), KiloAutoReliabilityEnabled: true})
	waits := 0
	base.retryWait = func(context.Context, time.Duration) error {
		waits++
		return nil
	}
	base.retryJitter = func(time.Duration) time.Duration { return 0 }
	handler := newLingJSONCompatHandler(base, base.cfg.MaxBodyBytes)

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK || calls != 2 || waits != 1 || lingJSONTestResponseContent(t, rec.Body.Bytes()) != "OK" {
		t.Fatalf("status=%d calls=%d waits=%d body=%s", rec.Code, calls, waits, rec.Body.String())
	}
}

func TestKiloAutoTransientRetryStopsOnCancellation(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	base := newBaseHandler(Config{UpstreamBaseURL: upstream.URL, RequestLogDir: t.TempDir(), KiloAutoReliabilityEnabled: true})
	waits := 0
	base.retryWait = func(waitCtx context.Context, _ time.Duration) error {
		waits++
		cancel()
		return waitCtx.Err()
	}
	base.retryJitter = func(time.Duration) time.Duration { return 0 }
	handler := newLingJSONCompatHandler(base, base.cfg.MaxBodyBytes)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError || calls != 1 || waits != 1 {
		t.Fatalf("status=%d calls=%d waits=%d body=%s", rec.Code, calls, waits, rec.Body.String())
	}
}

func TestKiloAutoSharesRawRetryBudgetWithSemanticRepair(t *testing.T) {
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1, 2, 4:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"PRIVATE_TRANSIENT"}}`)
		case 3:
			writeKiloAutoCompletion(t, w, "", "PRIVATE_EMPTY", "length", nil)
		default:
			t.Fatalf("unexpected upstream call %d", calls)
		}
	}))
	defer upstream.Close()

	base := newBaseHandler(Config{UpstreamBaseURL: upstream.URL, RequestLogDir: t.TempDir(), KiloAutoReliabilityEnabled: true})
	waits := 0
	base.retryWait = func(context.Context, time.Duration) error {
		waits++
		return nil
	}
	base.retryJitter = func(time.Duration) time.Duration { return 0 }
	jsonCompat := newLingJSONCompatHandler(base, base.cfg.MaxBodyBytes)
	handler := newKiloAutoReliabilityHandler(jsonCompat, base.cfg.MaxBodyBytes)

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"write code"}]}`)
	if rec.Code != http.StatusInternalServerError || calls != 4 || waits != 2 {
		t.Fatalf("status=%d calls=%d waits=%d body=%s", rec.Code, calls, waits, rec.Body.String())
	}
}
