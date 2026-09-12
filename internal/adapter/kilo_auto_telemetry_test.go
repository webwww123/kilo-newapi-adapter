package adapter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const telemetryNex = "nex-agi/nex-n2.5-pro:free"

func telemetryTestHandler(t *testing.T, responder func(int, *http.Request) (int, string), reliable bool) (*kiloAutoTelemetryHandler, <-chan kiloAutoTelemetryRecord) {
	t.Helper()
	var calls atomic.Int32
	client := &http.Client{Transport: kiloAutoRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, body := responder(int(calls.Add(1)), r)
		if status == 0 {
			return nil, errors.New("PRIVATE_TRANSPORT_SECRET")
		}
		contentType := "application/json"
		if strings.HasPrefix(body, "data:") {
			contentType = "text/event-stream"
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})}
	base := newBaseHandler(Config{UpstreamBaseURL: "http://upstream.invalid", HTTPClient: client, RequestLogDir: t.TempDir(), RequestLogBodyLimit: 1, KiloAutoReliabilityEnabled: reliable})
	base.retryWait = func(context.Context, time.Duration) error { return nil }
	base.retryJitter = func(time.Duration) time.Duration { return 0 }
	var handler http.Handler = newLingJSONCompatHandler(base, base.cfg.MaxBodyBytes)
	if reliable {
		handler = newKiloAutoReliabilityHandler(handler, base.cfg.MaxBodyBytes)
	}
	telemetry := newKiloAutoTelemetryHandler(handler)
	records := make(chan kiloAutoTelemetryRecord, 10)
	telemetry.emit = func(record kiloAutoTelemetryRecord) { records <- record }
	return telemetry, records
}

func TestKiloAutoTelemetryDefaultOffAndModelScope(t *testing.T) {
	t.Setenv("KILO_AUTO_TELEMETRY_ENABLED", "")
	if ConfigFromEnv().KiloAutoTelemetryEnabled {
		t.Fatal("telemetry enabled by default")
	}
	t.Setenv("KILO_AUTO_TELEMETRY_ENABLED", "true")
	if !ConfigFromEnv().KiloAutoTelemetryEnabled {
		t.Fatal("telemetry opt-in missing")
	}
	h, records := telemetryTestHandler(t, func(int, *http.Request) (int, string) {
		return 200, jsonCompatTestCompletion(t, telemetryNex, "OK")
	}, false)
	postJSON(t, h, "/v1/chat/completions", `{"model":"kilo/other","messages":[{"role":"user","content":"PRIVATE_PROMPT"}]}`)
	if len(records) != 0 {
		t.Fatal("sibling emitted Auto telemetry")
	}
}

func TestKiloAutoTelemetryPrivacyAndOriginalModel(t *testing.T) {
	h, records := telemetryTestHandler(t, func(int, *http.Request) (int, string) {
		return 200, `{"model":"nex-agi/nex-n2.5-pro:free","provider_metadata":{"routing":{"model":"dots-studio/dots-3-note-preview:free","error":"PRIVATE_ERROR"}},"choices":[{"message":{"content":"PRIVATE_ANSWER","reasoning":"PRIVATE_REASONING"},"finish_reason":"stop"}]}`
	}, true)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"PRIVATE_PROMPT"}],"max_tokens":64}`))
	r.Header.Set("Authorization", "Bearer PRIVATE_KEY")
	r.Header.Set("Cookie", "PRIVATE_COOKIE")
	r.Header.Set("X-Request-Id", "PRIVATE_REQUEST_ID")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	x := <-records
	if len(x.Attempts) != 1 || x.Attempts[0].Model != telemetryNex || x.Outcome != "visible_complete" || x.OutputLimit != 64 {
		t.Fatalf("record=%+v attempts=%+v", x, x.Attempts)
	}
	if strings.Contains(w.Body.String(), telemetryNex) || strings.Contains(w.Body.String(), "PRIVATE_ERROR") || !strings.Contains(w.Body.String(), "kilo/kilo-auto/free") {
		t.Fatal("public model masking changed")
	}
	encoded, _ := json.Marshal(x)
	if strings.Contains(string(encoded), "PRIVATE_") || x.RequestHash == "" || x.TraceID == "" {
		t.Fatalf("unsafe or missing metadata: %s", encoded)
	}
}

func TestKiloAutoTelemetryRawRetryAndSemanticRepairStaySeparate(t *testing.T) {
	h, records := telemetryTestHandler(t, func(n int, r *http.Request) (int, string) {
		switch n {
		case 1:
			return 429, `{"error":{"message":"PRIVATE_RATE_LIMIT"}}`
		case 2:
			return 200, `{"model":"dots-studio/dots-3-note-preview:free","choices":[{"message":{"content":""},"finish_reason":"stop"}]}`
		default:
			return 200, jsonCompatTestCompletion(t, telemetryNex, "OK")
		}
	}, true)
	w := postJSON(t, h, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"hi"}]}`)
	x := <-records
	if w.Code != 200 || len(x.Attempts) != 3 || x.Reliability == nil || !x.Reliability.Used || x.Reliability.FirstFailure != "empty_visible_answer" || x.Outcome != "visible_complete" {
		t.Fatalf("incorrect repair record=%+v", x)
	}
	if x.Attempts[0].Status != 429 || x.Attempts[0].Model != "unknown" || !x.Attempts[1].RawRetry || x.Attempts[2].RawRetry || x.Attempts[2].Stage != "visible_repair" || x.Attempts[2].Model != telemetryNex {
		t.Fatalf("incorrect attempt grouping: %+v %+v %+v", x.Attempts[0], x.Attempts[1], x.Attempts[2])
	}
}

func TestKiloAutoTelemetryTransportAndUnknownModel(t *testing.T) {
	h, records := telemetryTestHandler(t, func(n int, _ *http.Request) (int, string) {
		if n == 1 {
			return 0, ""
		}
		return 200, jsonCompatTestCompletion(t, "PRIVATE_NEW_MODEL", "OK")
	}, true)
	postJSON(t, h, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"hi"}]}`)
	x := <-records
	if len(x.Attempts) != 2 || !x.Attempts[0].TransportError || x.Attempts[1].Model != "unknown" {
		t.Fatalf("record=%+v", x)
	}
	encoded, _ := json.Marshal(x)
	if strings.Contains(string(encoded), "PRIVATE_") {
		t.Fatal("untrusted model or transport error leaked")
	}
}

func TestKiloAutoTelemetryJSONRepair(t *testing.T) {
	h, records := telemetryTestHandler(t, func(n int, _ *http.Request) (int, string) {
		text := "not JSON"
		if n == 2 {
			text = `{"ok":true}`
		}
		return 200, jsonCompatTestCompletion(t, telemetryNex, text)
	}, true)
	w := postJSON(t, h, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`)
	x := <-records
	if w.Code != 200 || x.JSON == nil || !x.JSON.RepairUsed || !x.JSON.Validated || x.JSON.ObjectFallback || len(x.Attempts) != 2 || x.Attempts[1].Stage != "json_repair" || x.Outcome != "json_validated" {
		t.Fatalf("record=%+v", x)
	}
}

func TestKiloAutoTelemetryDoesNotCountRoutingCandidatesOrUserJSON(t *testing.T) {
	for _, body := range []string{
		`{"model":"kilo-auto/free","provider_metadata":{"model":"nex-agi/nex-n2.5-pro:free"},"choices":[{"message":{"content":"{\"model\":\"nex-agi/nex-n2.5-pro:free\"}"},"finish_reason":"stop"}]}`,
		`{"model":"PRIVATE_KEY","choices":[{"message":{"tool_calls":[{"function":{"arguments":"{\"model\":\"nex-agi/nex-n2.5-pro:free\"}"}}]},"finish_reason":"tool_calls"}]}`,
	} {
		a := &kiloAutoTelemetryAttempt{Model: "unknown"}
		var payload map[string]any
		_ = json.Unmarshal([]byte(body), &payload)
		a.observe(payload)
		if a.Model != "unknown" {
			t.Fatalf("inferred selected model from opaque content: %+v", a)
		}
	}
	a := &kiloAutoTelemetryAttempt{Model: "unknown"}
	a.observe(map[string]any{"model": telemetryNex})
	a.observe(map[string]any{"model": "poolside/laguna-s-2.1:free"})
	if a.Model != "unknown" || !a.ModelConflict {
		t.Fatal("conflicting model evidence was hidden")
	}
	a = &kiloAutoTelemetryAttempt{Model: "unknown"}
	a.observe(map[string]any{"model": telemetryNex})
	a.observe(map[string]any{"model": "PRIVATE_UNREVIEWED_MODEL"})
	a.observe(map[string]any{"model": telemetryNex})
	if a.Model != "unknown" || !a.UnknownModel {
		t.Fatal("unreviewed model evidence was overwritten")
	}
}

func TestKiloAutoTelemetryJSONObjectFallbackIsNotUpstreamSuccess(t *testing.T) {
	h, records := telemetryTestHandler(t, func(int, *http.Request) (int, string) {
		return 200, jsonCompatTestCompletion(t, telemetryNex, "plain text, not JSON")
	}, true)
	w := postJSON(t, h, "/v1/chat/completions", `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_object"}}`)
	x := <-records
	if w.Code != 200 || x.JSON == nil || !x.JSON.ObjectFallback || x.Outcome != "local_object_fallback" {
		t.Fatalf("local fallback classified as upstream JSON success: %+v", x)
	}
}

func TestKiloAutoTelemetryProtocolEnvelopesAndIncompleteStreams(t *testing.T) {
	for _, tc := range []struct{ path, body, outcome string }{
		{"/v1/messages", `{"type":"message","model":"nex-agi/nex-n2.5-pro:free","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`, "terminal_observed"},
		{"/v1/responses", `{"model":"nex-agi/nex-n2.5-pro:free","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"OK"}]}]}`, "terminal_observed"},
		{"/v1/chat/completions", "data: {\"model\":\"nex-agi/nex-n2.5-pro:free\",\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n", "unverified"},
	} {
		t.Run(tc.path+tc.outcome, func(t *testing.T) {
			h, records := telemetryTestHandler(t, func(int, *http.Request) (int, string) { return 200, tc.body }, false)
			postJSON(t, h, tc.path, `{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"hi"}],"input":"hi","max_tokens":64,"stream":true}`)
			x := <-records
			if len(x.Attempts) != 1 || x.Attempts[0].Model != telemetryNex || x.Outcome != tc.outcome {
				t.Fatalf("record=%+v attempt=%+v", x, x.Attempts)
			}
		})
	}
}

func TestKiloAutoTelemetryFlushBeforeCompletion(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"model\":\"nex-agi/nex-n2.5-pro:free\",\"choices\":[{\"delta\":{\"content\":\"FIRST\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	h := NewHandler(Config{UpstreamBaseURL: upstream.URL, KiloAutoTelemetryEnabled: true, KiloAutoReliabilityEnabled: true, RequestLogDir: t.TempDir(), RequestLogBodyLimit: 1}).(*kiloAutoTelemetryHandler)
	records := make(chan kiloAutoTelemetryRecord, 1)
	h.emit = func(x kiloAutoTelemetryRecord) { records <- x }
	adapter := httptest.NewServer(h)
	defer adapter.Close()
	defer unblock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, adapter.URL+"/v1/chat/completions", strings.NewReader(`{"model":"kilo/kilo-auto/free","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	resp, err := adapter.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "FIRST") {
		t.Fatalf("stream was buffered: %q %v", line, err)
	}
	if len(records) != 0 {
		t.Fatal("emitted final statistics before stream finished")
	}
	unblock()
	_, _ = io.ReadAll(reader)
	select {
	case x := <-records:
		if x.Outcome != "terminal_observed" || len(x.Attempts) != 1 || x.Attempts[0].Model != telemetryNex {
			t.Fatalf("record=%+v", x)
		}
	case <-ctx.Done():
		t.Fatal("missing final statistics")
	}
}

func TestKiloAutoTelemetryBoundedAndCancelled(t *testing.T) {
	trace := &kiloAutoTelemetry{active: true, stage: "initial"}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), kiloAutoTelemetryKey{}, trace))
	for i := 0; i < 20; i++ {
		beginKiloAutoTelemetryAttempt(ctx)
	}
	if len(trace.record.Attempts) != 8 || trace.record.Dropped != 12 {
		t.Fatal("attempt storage is unbounded")
	}
	cancel()
	if trace.outcome(ctx) != "cancelled" {
		t.Fatal("cancellation counted as upstream failure")
	}
}
