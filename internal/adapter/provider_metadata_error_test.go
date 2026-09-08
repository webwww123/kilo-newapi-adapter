package adapter

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestProviderRoutingFailuresPreserveSuccessfulResults(t *testing.T) {
	tests := []struct {
		name, path, request, response string
		preserved                     []string
	}{
		{
			name: "chat", path: "/v1/chat/completions",
			request:   `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}]}`,
			response:  `{"id":"chat_1","choices":[{"message":{"role":"assistant","content":"visible answer","provider_metadata":{"gateway":{"routing":{"attempts":[{"error":{"message":"private routing failure"}}]}}},"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"error\":\"customer value\"}"}}]},"finish_reason":"tool_calls"}]}`,
			preserved: []string{"visible answer", `"id":"call_1"`, `"name":"lookup"`, `"arguments":"{\"error\":\"customer value\"}"`, `"finish_reason":"tool_calls"`},
		},
		{
			name: "messages", path: "/v1/messages",
			request:   `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`,
			response:  `{"id":"msg_1","type":"message","role":"assistant","providerMetadata":{"routing":{"attempts":[{"type":"error","error":"private routing failure"}]}},"content":[{"type":"text","text":"visible answer"},{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"error":"customer value"}}],"stop_reason":"tool_use"}`,
			preserved: []string{"visible answer", `"id":"toolu_1"`, `"input":{"error":"customer value"}`, `"stop_reason":"tool_use"`},
		},
		{
			name: "responses", path: "/v1/responses",
			request:   `{"model":"kilo/public-model","input":"hi"}`,
			response:  `{"id":"resp_1","object":"response","status":"completed","provider_metadata":{"routing":{"attempts":[{"error":"private routing failure"}]}},"output":[{"type":"message","content":[{"type":"output_text","text":"visible answer"}]},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"error\":\"customer value\"}"}]}`,
			preserved: []string{"visible answer", `"id":"fc_1"`, `"call_id":"call_1"`, `"arguments":"{\"error\":\"customer value\"}"`, `"status":"completed"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.response)
			}))
			defer upstream.Close()
			logDir := t.TempDir()
			handler := NewHandler(Config{UpstreamBaseURL: upstream.URL, RequestLogDir: logDir})
			rec := postJSON(t, handler, tc.path, tc.request)
			if rec.Code != http.StatusOK || calls.Load() != 1 {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
			}
			assertProviderRoutingResult(t, rec.Body.String(), tc.preserved)
			assertProviderRoutingLog(t, logDir, http.StatusOK, 1)
		})
	}
}

func TestProviderRoutingFailuresPreserveSuccessfulStreams(t *testing.T) {
	tests := []struct {
		name, path, request, stream string
		preserved                   []string
	}{
		{
			name: "chat", path: "/v1/chat/completions",
			request:   `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			stream:    "data: " + `{"choices":[{"delta":{"content":"visible answer","provider_metadata":{"routing":{"attempts":[{"error":"private routing failure"}]}},"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n",
			preserved: []string{"visible answer", `"id":"call_1"`, `"arguments":"{}"`, `"finish_reason":"tool_calls"`, "data: [DONE]"},
		},
		{
			name: "messages", path: "/v1/messages",
			request:   `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"stream":true}`,
			stream:    "event: content_block_start\ndata: " + `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"lookup","input":{}},"providerMetadata":{"routing":{"attempts":[{"error":"private routing failure"}]}}}` + "\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			preserved: []string{"event: content_block_start", `"id":"toolu_1"`, `"input":{}`, "event: message_stop"},
		},
		{
			name: "responses", path: "/v1/responses",
			request:   `{"model":"kilo/public-model","input":"hi","stream":true}`,
			stream:    "event: response.completed\ndata: " + `{"type":"response.completed","response":{"id":"resp_1","status":"completed","provider_metadata":{"routing":{"attempts":[{"error":"private routing failure"}]}},"output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{}"}]}}` + "\n\n",
			preserved: []string{"event: response.completed", `"status":"completed"`, `"id":"fc_1"`, `"call_id":"call_1"`, `"arguments":"{}"`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.stream)
			}))
			defer upstream.Close()
			logDir := t.TempDir()
			rec := postJSON(t, NewHandler(Config{UpstreamBaseURL: upstream.URL, RequestLogDir: logDir}), tc.path, tc.request)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			assertProviderRoutingResult(t, rec.Body.String(), tc.preserved)
			assertProviderRoutingLog(t, logDir, http.StatusOK, 1)
			audit := copyEventStreamWithNormalizeAndCapture(httptest.NewRecorder(), strings.NewReader(tc.stream), nil, "kilo/public-model", strings.TrimPrefix(tc.path, "/v1"))
			if audit.HadError || audit.ErrorEvents != 0 || audit.ProviderRoutingErrorEvents != 1 {
				t.Fatalf("audit=%+v", audit)
			}
		})
	}
}

func TestProviderRoutingMetadataDoesNotHideActualErrors(t *testing.T) {
	for _, payload := range []string{
		`{"error":{"message":"actual failure"},"provider_metadata":{"error":"private routing failure"}}`,
		`{"type":"error","providerMetadata":{"error":"private routing failure"}}`,
		`{"response":{"status":"failed","error":{"message":"actual failure"},"provider_metadata":{"error":"private routing failure"}}}`,
	} {
		for _, path := range []string{"/chat/completions", "/messages", "/responses"} {
			body, valid := normalizeSuccessResponseBodyChecked([]byte(payload), "kilo/public-model", path, false)
			if valid || !strings.Contains(string(body), publicServerErrorMessage) {
				t.Fatalf("actual error accepted: path=%s body=%s", path, body)
			}
			audit := copyEventStreamWithNormalizeAndCapture(httptest.NewRecorder(), strings.NewReader("data: "+payload+"\n\n"), nil, "kilo/public-model", path)
			if !audit.HadError || audit.ErrorEvents != 1 || audit.ProviderRoutingErrorEvents != 1 {
				t.Fatalf("actual stream error lost: %+v", audit)
			}
		}
	}
	var opaque any
	_ = json.Unmarshal([]byte(`{"content":[{"type":"tool_use","input":{"provider_metadata":{"error":"customer value"}}}]}`), &opaque)
	if successPayloadHasError(opaque) || successPayloadHasProviderRoutingError(opaque) {
		t.Fatal("customer tool input classified as an upstream failure")
	}
}

func assertProviderRoutingResult(t *testing.T, body string, preserved []string) {
	t.Helper()
	for _, marker := range preserved {
		if !strings.Contains(body, marker) {
			t.Fatalf("lost %q: %s", marker, body)
		}
	}
	for _, marker := range []string{"private routing failure", "provider_metadata", "providerMetadata", publicServerErrorMessage} {
		if strings.Contains(body, marker) {
			t.Fatalf("unexpected %q: %s", marker, body)
		}
	}
}

func assertProviderRoutingLog(t *testing.T, dir string, status, events int) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "requests-*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("log files=%v err=%v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var entry requestLogEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Status != status || entry.ProviderRoutingErrorEvents != events {
		t.Fatalf("status=%d routing_error_events=%d", entry.Status, entry.ProviderRoutingErrorEvents)
	}
}
