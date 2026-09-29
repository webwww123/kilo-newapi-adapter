package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContextRecallPrefersPlatformRequestID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recall.json")
	if err := os.WriteFile(path, []byte(`{"mode":"apply","percent":100}`), 0600); err != nil {
		t.Fatal(err)
	}
	c := newContextRecallControl(path)
	for _, row := range []struct{ platform, physical, want string }{
		{"logical-123", "attempt-456", "logical-123"},
		{"", "attempt-456", "attempt-456"},
		{"", "", ""},
		{strings.Repeat("x", 1025), "attempt-456", ""},
	} {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Header.Set("X-Oneapi-Request-Id", row.platform)
		r.Header.Set("X-Request-Id", row.physical)
		want := ""
		if row.want != "" {
			digest := sha256.Sum256([]byte(row.want))
			want = hex.EncodeToString(digest[:16])
		}
		if q := c.begin(r); q == nil || q.event.RequestHash != want {
			t.Fatal("logical request correlation mismatch")
		}
		if r.Header.Get("X-Oneapi-Request-Id") != row.platform || r.Header.Get("X-Request-Id") != row.physical {
			t.Fatal("telemetry mutated routing headers")
		}
	}
}

func TestContextRecallControlReloadAndFailOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recall.json")
	c := newContextRecallControl(path)
	if c.current().Percent != 0 {
		t.Fatal("missing file enabled recall")
	}
	for _, row := range []struct {
		body    string
		percent int
		mode    string
	}{
		{`{"mode":"shadow","percent":25}`, 25, "shadow"},
		{`{"mode":"apply","percent":100}`, 100, "apply"},
		{`{"mode":"apply","percent":0}`, 0, "apply"},
		{`{"mode":"apply","percent":101}`, 0, ""},
		{`{"mode":"typo","percent":100}`, 0, ""},
		{`{broken`, 0, ""},
		{strings.Repeat(" ", 2048), 0, ""},
	} {
		if err := os.WriteFile(path, []byte(row.body), 0600); err != nil {
			t.Fatal(err)
		}
		c.nextReadNS.Store(0)
		got := c.current()
		if got.Percent != row.percent || got.Mode != row.mode {
			t.Fatalf("control mismatch: %+v", got)
		}
	}
	if err := os.WriteFile(path, []byte(`{"mode":"apply","percent":100}`), 0600); err != nil {
		t.Fatal(err)
	}
	c.nextReadNS.Store(0)
	r := httptest.NewRequest("POST", "/v1/messages", nil)
	r.Header.Set("X-Request-Id", "synthetic-private-request-id")
	q := c.begin(r)
	if q == nil || !q.chosen || q.event.Protocol != "messages" || q.event.RequestHash == "" || strings.Contains(q.event.RequestHash, "synthetic") {
		t.Fatal("sampling/protocol/private correlation failed")
	}
	var events []contextRecallEvent
	c.emit = func(e contextRecallEvent) { events = append(events, e) }
	q.finish(200, time.Second)
	if len(events) != 0 {
		t.Fatal("untrimmed request emitted recall telemetry")
	}
	q.triggered = true
	q.finish(200, time.Second)
	if len(events) != 1 || events[0].DurationMS != 1000 {
		t.Fatal("eligible request telemetry missing")
	}
}
