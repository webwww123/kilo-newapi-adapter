package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type upstreamCall struct {
	Method string
	Path   string
	Header http.Header
	Body   map[string]any
}

func newRecordingUpstream(t *testing.T) (*httptest.Server, *[]upstreamCall) {
	t.Helper()

	var calls []upstreamCall
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := upstreamCall{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   map[string]any{},
		}

		if r.Body != nil {
			defer r.Body.Close()
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read upstream body: %v", err)
			}
			if len(bytes.TrimSpace(body)) > 0 {
				if err := json.Unmarshal(body, &call.Body); err != nil {
					t.Fatalf("decode upstream body: %v", err)
				}
			}
		}
		calls = append(calls, call)

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"real/free","object":"model","name":"Real Free"}]}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))

	return server, &calls
}

func newTestHandler(upstreamURL string) http.Handler {
	cfg := Config{
		UpstreamBaseURL: upstreamURL,
	}
	return NewHandler(cfg)
}

func postJSON(t *testing.T, handler http.Handler, path string, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestProxyRoutesStandardAndNonV1Paths(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)

	cases := []struct {
		name         string
		method       string
		path         string
		body         string
		upstreamPath string
	}{
		{
			name:         "chat completions",
			method:       http.MethodPost,
			path:         "/v1/chat/completions",
			body:         `{"model":"some/model","messages":[{"role":"user","content":"hi"}]}`,
			upstreamPath: "/chat/completions",
		},
		{
			name:         "responses",
			method:       http.MethodPost,
			path:         "/v1/responses",
			body:         `{"model":"kilo/real/free","input":"hi"}`,
			upstreamPath: "/responses",
		},
		{
			name:         "messages",
			method:       http.MethodPost,
			path:         "/v1/messages",
			body:         `{"model":"unknown-model","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`,
			upstreamPath: "/messages",
		},
		{
			name:         "non-v1 chat completions",
			method:       http.MethodPost,
			path:         "/chat/completions",
			body:         `{"messages":[{"role":"user","content":"hi"}]}`,
			upstreamPath: "/chat/completions",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := len(*calls)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if len(*calls) != before+1 {
				t.Fatalf("upstream calls = %d, want %d", len(*calls), before+1)
			}

			call := (*calls)[before]
			if call.Path != tc.upstreamPath {
				t.Fatalf("upstream path = %q, want %q", call.Path, tc.upstreamPath)
			}
		})
	}

	if (*calls)[0].Body["model"] != "some/model" {
		t.Fatalf("model should pass through unchanged: %#v", (*calls)[0].Body["model"])
	}
	if (*calls)[1].Body["model"] != "real/free" {
		t.Fatalf("kilo-prefixed model should be stripped before upstream: %#v", (*calls)[1].Body["model"])
	}
	if (*calls)[2].Body["model"] != "unknown-model" {
		t.Fatalf("unknown model should pass through unchanged: %#v", (*calls)[2].Body["model"])
	}
	if _, ok := (*calls)[3].Body["model"]; ok {
		t.Fatalf("missing model should remain missing, got: %#v", (*calls)[3].Body["model"])
	}
}

func TestProxySanitizesClientHeadersAndAddsUpstreamAuth(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	cfg := Config{
		UpstreamBaseURL: upstream.URL,
		UpstreamAPIKey:  "upstream-token",
	}
	handler := NewHandler(cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"some/model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Cookie", "session=client")
	req.Header.Set("X-Provider-Base-URL", "https://example.invalid")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(*calls))
	}

	header := (*calls)[0].Header
	if got := header.Get("Authorization"); got != "Bearer upstream-token" {
		t.Fatalf("authorization = %q, want upstream bearer", got)
	}
	if got := header.Get("Cookie"); got != "" {
		t.Fatalf("cookie leaked upstream: %q", got)
	}
	if got := header.Get("X-Provider-Base-URL"); got != "" {
		t.Fatalf("provider override leaked upstream: %q", got)
	}
}

func TestModelsPrefixesUpstreamIDsForPublicListing(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 1 || (*calls)[0].Path != "/models" {
		t.Fatalf("models request was not proxied correctly: %#v", *calls)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode models response: %v", err)
	}

	if len(payload.Data) != 1 {
		t.Fatalf("model count = %d, want 1: %s", len(payload.Data), rec.Body.String())
	}
	if payload.Data[0].ID != "kilo/real/free" {
		t.Fatalf("public model id = %q, want kilo/real/free", payload.Data[0].ID)
	}
}

func TestCountTokensReturnsLocalAnthropicShape(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	rec := postJSON(t, handler, "/v1/messages/count_tokens", `{"model":"some/model","messages":[{"role":"user","content":"hello world"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("count_tokens should not call upstream, got %#v", *calls)
	}

	var payload struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode count tokens: %v", err)
	}
	if payload.InputTokens <= 0 {
		t.Fatalf("input_tokens = %d, want positive", payload.InputTokens)
	}
}

func TestFreeChatCompletionContextIsTrimmedBeforeForwarding(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	oldContent := "DROP_OLD_CONTEXT " + strings.Repeat("old ", 160000)
	rec := postJSON(t, handler, "/v1/chat/completions", `{
		"model":"kilo/stepfun/step-3.7-flash:free",
		"messages":[
			{"role":"system","content":"KEEP_SYSTEM"},
			{"role":"user","content":"`+oldContent+`"},
			{"role":"assistant","content":"`+strings.Repeat("assistant ", 160000)+`"},
			{"role":"user","content":"KEEP_LATEST_USER"}
		],
		"max_tokens":8192
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(*calls))
	}
	body := (*calls)[0].Body
	if body["model"] != "stepfun/step-3.7-flash:free" {
		t.Fatalf("model = %#v, want stripped free model", body["model"])
	}
	messages := body["messages"].([]any)
	if !messagesContainText(messages, "KEEP_SYSTEM") {
		t.Fatalf("trimmed messages dropped system message; len=%d", len(messages))
	}
	if !messagesContainText(messages, "KEEP_LATEST_USER") {
		t.Fatalf("trimmed messages dropped latest user; len=%d", len(messages))
	}
	if messagesContainText(messages, "DROP_OLD_CONTEXT") {
		t.Fatalf("trimmed messages kept old context; len=%d", len(messages))
	}
}

func TestPaidChatCompletionContextIsNotTrimmed(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	oldContent := "KEEP_PAID_OLD_CONTEXT " + strings.Repeat("old ", 160000)
	rec := postJSON(t, handler, "/v1/chat/completions", `{
		"model":"kilo/deepseek/deepseek-v4-pro",
		"messages":[
			{"role":"user","content":"`+oldContent+`"},
			{"role":"assistant","content":"`+strings.Repeat("assistant ", 160000)+`"},
			{"role":"user","content":"KEEP_PAID_LATEST"}
		],
		"max_tokens":8192
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	messages := (*calls)[0].Body["messages"].([]any)
	if !messagesContainText(messages, "KEEP_PAID_OLD_CONTEXT") {
		t.Fatalf("paid model context was unexpectedly trimmed; len=%d", len(messages))
	}
}

func TestFreeResponsesContextIsTrimmedBeforeForwarding(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	rec := postJSON(t, handler, "/v1/responses", `{
		"model":"kilo/stepfun/step-3.7-flash:free",
		"input":[
			{"role":"system","content":"KEEP_RESPONSE_SYSTEM"},
			{"role":"user","content":"DROP_RESPONSE_OLD `+strings.Repeat("old ", 160000)+`"},
			{"role":"assistant","content":"`+strings.Repeat("assistant ", 160000)+`"},
			{"role":"user","content":"KEEP_RESPONSE_LATEST"}
		],
		"max_output_tokens":8192
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	input := (*calls)[0].Body["input"].([]any)
	if !messagesContainText(input, "KEEP_RESPONSE_SYSTEM") {
		t.Fatalf("trimmed input dropped system item; len=%d", len(input))
	}
	if !messagesContainText(input, "KEEP_RESPONSE_LATEST") {
		t.Fatalf("trimmed input dropped latest user item; len=%d", len(input))
	}
	if messagesContainText(input, "DROP_RESPONSE_OLD") {
		t.Fatalf("trimmed input kept old response context; len=%d", len(input))
	}
}

func TestHealthz(t *testing.T) {
	handler := newTestHandler("http://127.0.0.1:1")
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "ok" {
		t.Fatalf("health body = %q", rec.Body.String())
	}
}

func messagesContainText(messages []any, text string) bool {
	for _, item := range messages {
		if strings.Contains(fmt.Sprint(item), text) {
			return true
		}
	}
	return false
}
