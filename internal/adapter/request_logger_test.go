package adapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRequestLoggerRecordsProxiedRequestAndResponse(t *testing.T) {
	upstream, _ := newRecordingUpstream(t)
	defer upstream.Close()

	logDir := t.TempDir()
	handler := NewHandler(Config{
		UpstreamBaseURL:     upstream.URL,
		RequestLogDir:       logDir,
		RequestLogRetention: 48 * time.Hour,
		RequestLogBodyLimit: 2048,
	})

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"some/free","messages":[{"role":"user","content":"hello"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	files, err := filepath.Glob(filepath.Join(logDir, "requests-*.jsonl"))
	if err != nil {
		t.Fatalf("glob logs: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("log files = %v, want one request log", files)
	}

	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read request log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, data = %s", len(lines), string(data))
	}

	var entry requestLogEntry
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("decode request log: %v\n%s", err, lines[0])
	}
	if entry.Method != "POST" || entry.Path != "/v1/chat/completions" {
		t.Fatalf("request identity = %s %s", entry.Method, entry.Path)
	}
	if entry.UpstreamPath != "/chat/completions" {
		t.Fatalf("upstream path = %q", entry.UpstreamPath)
	}
	if entry.Status != http.StatusOK {
		t.Fatalf("status = %d", entry.Status)
	}
	if entry.Request.Model != "some/free" {
		t.Fatalf("model = %q", entry.Request.Model)
	}
	if !strings.Contains(entry.Request.Body, `"content":"hello"`) {
		t.Fatalf("request body not recorded: %q", entry.Request.Body)
	}
	if !strings.Contains(entry.Response.Body, `"ok":true`) {
		t.Fatalf("response body not recorded: %q", entry.Response.Body)
	}
}

func TestRequestLoggerPreservesOriginalErrorWhenPublicResponseIsGeneric(t *testing.T) {
	const rawMarker = "model ling-3.0-flash: target label not found"
	const upstreamBody = `{"error":{"message":"Provider returned error","metadata":{"raw":"{\"message\":\"model ling-3.0-flash: target label not found trace_id: internal-trace\"}","provider_name":"InternalProvider","previous_errors":[{"code":429,"raw":"server overload"}]}}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	logDir := t.TempDir()
	handler := NewHandler(Config{
		UpstreamBaseURL:     upstream.URL,
		RequestLogDir:       logDir,
		RequestLogRetention: 48 * time.Hour,
		RequestLogBodyLimit: 2048,
	})

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/inclusionai/ling-3.0-flash:free","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), publicClientErrorMessage) {
		t.Fatalf("generic client error missing: %s", rec.Body.String())
	}
	for _, leaked := range []string{rawMarker, "InternalProvider", "internal-trace", "previous_errors", "raw"} {
		if strings.Contains(rec.Body.String(), leaked) {
			t.Fatalf("public response leaked %q: %s", leaked, rec.Body.String())
		}
	}

	files, err := filepath.Glob(filepath.Join(logDir, "requests-*.jsonl"))
	if err != nil {
		t.Fatalf("glob logs: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("log files = %v, want one request log", files)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatalf("read request log: %v", err)
	}
	var entry requestLogEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
		t.Fatalf("decode request log: %v", err)
	}
	if entry.Status != http.StatusBadRequest {
		t.Fatalf("internal log status = %d, want %d", entry.Status, http.StatusBadRequest)
	}
	for _, detail := range []string{rawMarker, "InternalProvider", "internal-trace", "previous_errors", "server overload"} {
		if !strings.Contains(entry.Response.Body, detail) {
			t.Fatalf("internal log lost %q: %s", detail, entry.Response.Body)
		}
	}
	if strings.Contains(entry.Response.Body, publicClientErrorMessage) {
		t.Fatalf("internal log stored only the public error: %s", entry.Response.Body)
	}
}

func TestRequestLoggerRemovesLogsOlderThanRetention(t *testing.T) {
	logDir := t.TempDir()
	oldPath := filepath.Join(logDir, "requests-2026-06-16.jsonl")
	recentPath := filepath.Join(logDir, "requests-2026-06-18.jsonl")
	if err := os.WriteFile(oldPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write old log: %v", err)
	}
	if err := os.WriteFile(recentPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write recent log: %v", err)
	}
	oldTime := time.Date(2026, 6, 16, 1, 0, 0, 0, time.UTC)
	recentTime := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(oldPath, oldTime, oldTime); err != nil {
		t.Fatalf("chtime old log: %v", err)
	}
	if err := os.Chtimes(recentPath, recentTime, recentTime); err != nil {
		t.Fatalf("chtime recent log: %v", err)
	}

	logger := newRequestLogger(RequestLogConfig{
		Dir:       logDir,
		Retention: 48 * time.Hour,
		BodyLimit: 1024,
		now: func() time.Time {
			return time.Date(2026, 6, 19, 12, 0, 0, 0, time.UTC)
		},
	})
	if err := logger.cleanup(); err != nil {
		t.Fatalf("cleanup logs: %v", err)
	}

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old log still exists or stat failed: %v", err)
	}
	if _, err := os.Stat(recentPath); err != nil {
		t.Fatalf("recent log missing: %v", err)
	}
}
