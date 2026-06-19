package adapter

import (
	"encoding/json"
	"net/http"
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
