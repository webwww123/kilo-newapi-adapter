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
}

func TestProxyRejectsMissingOrInvalidModelLocally(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	tests := []struct {
		name string
		body string
	}{
		{name: "missing", body: `{"messages":[{"role":"user","content":"hi"}]}`},
		{name: "empty", body: `{"model":"   ","messages":[{"role":"user","content":"hi"}]}`},
		{name: "prefix only", body: `{"model":"kilo/","messages":[{"role":"user","content":"hi"}]}`},
		{name: "wrong type", body: `{"model":42,"messages":[{"role":"user","content":"hi"}]}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, handler, "/v1/chat/completions", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"type":"invalid_request_error"`) {
				t.Fatalf("unexpected error body: %s", rec.Body.String())
			}
		})
	}

	if len(*calls) != 0 {
		t.Fatalf("invalid model requests reached upstream: %#v", *calls)
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
	req.Header.Set("Accept-Encoding", "gzip")
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
	if got := header.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("accept-encoding = %q, want identity for response sanitization", got)
	}
}

func TestProxyNormalizesLingUnsupportedControls(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()
	handler := newTestHandler(upstream.URL)
	body := `{"model":"kilo/inclusionai/ling-3.0-flash:free","messages":[{"role":"user","content":"give data"}],"reasoning_effort":"high","response_format":{"type":"json_object"},"max_tokens":4096,"tools":[{"type":"function","function":{"name":"lookup"}}]}`

	rec := postJSON(t, handler, "/v1/chat/completions", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("upstream calls=%d, want 1", len(*calls))
	}

	call := (*calls)[0]
	if _, exists := call.Body["response_format"]; exists {
		t.Fatalf("response_format reached upstream: %#v", call.Body)
	}
	if _, exists := call.Body["reasoning_effort"]; exists {
		t.Fatalf("reasoning_effort reached upstream: %#v", call.Body)
	}
	if call.Body["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens changed: %#v", call.Body)
	}
	if len(call.Body["messages"].([]any)) != 1 || len(call.Body["tools"].([]any)) != 1 {
		t.Fatalf("messages or tools changed: %#v", call.Body)
	}
}

func TestProxyRejectsUnsupportedLingInputLocally(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()
	handler := newTestHandler(upstream.URL)

	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/inclusionai/ling-3.0-flash:free","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}]}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "当前模型不支持图片输入") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("unsupported input reached upstream: %#v", *calls)
	}
}

func TestProxyRejectsLingEmbeddingsLocally(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()
	handler := newTestHandler(upstream.URL)

	rec := postJSON(t, handler, "/v1/embeddings", `{"model":"kilo/inclusionai/ling-3.0-flash:free","input":"hello"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "当前模型不支持向量嵌入") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("embeddings request reached upstream: %#v", *calls)
	}
}

func TestProxyRejectsLingContextThatCannotBeTrimmedSafely(t *testing.T) {
	withContextTrimProfile(t, lingFlashFreeModel, contextTrimProfile{ContextLimit: 3000})
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()
	handler := newTestHandler(upstream.URL)

	body := fmt.Sprintf(`{
		"model":"kilo/%s",
		"messages":[
			{"role":"system","content":"keep"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
			{"role":"user","content":%q}
		],
		"max_tokens":512
	}`, lingFlashFreeModel, strings.Repeat("x", 12000))
	rec := postJSON(t, handler, "/v1/chat/completions", body)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "上下文长度已超限") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(*calls) != 0 {
		t.Fatalf("unsafe oversized context reached upstream: %#v", *calls)
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

func TestModelsPrefixesNestedModelReferencesForPublicListing(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"object":"list",
			"data":[{
				"id":"kilo-auto/free",
				"canonical_slug":"stepfun/step-3.7-flash:free",
				"autoRouting":{"models":["stepfun/step-3.7-flash:free","tencent/hy3:free","kilo/already-prefixed"]}
			}]
		}`))
	}))
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leaked := range []string{`"canonical_slug":"stepfun/step-3.7-flash:free"`, `"stepfun/step-3.7-flash:free"`, `"tencent/hy3:free"`} {
		if strings.Contains(body, leaked) {
			t.Fatalf("models response leaked unprefixed model reference %q in body: %s", leaked, body)
		}
	}
	for _, expected := range []string{`"canonical_slug":"kilo/stepfun/step-3.7-flash:free"`, `"kilo/stepfun/step-3.7-flash:free"`, `"kilo/tencent/hy3:free"`, `"kilo/already-prefixed"`} {
		if !strings.Contains(body, expected) {
			t.Fatalf("models response missing prefixed model reference %q in body: %s", expected, body)
		}
	}
}

func TestProxyPrefixesSuccessResponseModelAndRemovesProvider(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","model":"tencent/hy3-20260706:free","provider":"Novita","choices":[{"message":{"role":"assistant","content":"OK"}}]}`))
	}))
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/tencent/hy3:free","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leaked := range []string{"tencent/hy3-20260706:free", `"provider":"Novita"`} {
		if strings.Contains(body, leaked) {
			t.Fatalf("success response leaked upstream detail %q in body: %s", leaked, body)
		}
	}
	if !strings.Contains(body, `"model":"kilo/tencent/hy3:free"`) {
		t.Fatalf("success response did not return public request model: %s", body)
	}
}

func TestProxyReturnsBadGatewayForHTTP200ErrorPayloads(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		requestBody  string
		upstreamBody string
		leaked       string
	}{
		{
			name:         "chat nested error",
			path:         "/v1/chat/completions",
			requestBody:  `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}]}`,
			upstreamBody: `{"error":{"message":"InternalProvider failed at https://internal.example"}}`,
			leaked:       "InternalProvider",
		},
		{
			name:         "messages error envelope",
			path:         "/v1/messages",
			requestBody:  `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`,
			upstreamBody: `{"type":"error","error":{"message":"real/upstream-model failed"}}`,
			leaked:       "real/upstream-model",
		},
		{
			name:         "responses invalid json",
			path:         "/v1/responses",
			requestBody:  `{"model":"kilo/public-model","input":"hi"}`,
			upstreamBody: `upstream returned invalid JSON`,
			leaked:       "invalid JSON",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.upstreamBody)
			}))
			defer upstream.Close()

			handler := newTestHandler(upstream.URL)
			rec := postJSON(t, handler, tc.path, tc.requestBody)

			if rec.Code != http.StatusBadGateway {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadGateway, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "upstream request failed") {
				t.Fatalf("generic upstream error missing: %s", rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), tc.leaked) {
				t.Fatalf("upstream detail leaked %q: %s", tc.leaked, rec.Body.String())
			}
		})
	}
}

func TestProxySanitizesSuccessJSONAcrossProtocols(t *testing.T) {
	publicModel := "kilo/public-model"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "7")
		w.Header().Set("Server", "upstream-server")
		w.Header().Set("Via", "internal-proxy")
		w.Header().Set("CF-Ray", "internal-ray")
		w.Header().Set("NEL", `{"report_to":"internal"}`)
		w.Header().Set("Report-To", `{"group":"internal"}`)
		w.Header().Set("X-Upstream-Proxy", "internal-proxy")
		w.Header().Set("X-Provider-Name", "InternalProvider")
		w.Header().Set("X-Request-ID", "internal-request")

		switch r.URL.Path {
		case "/chat/completions":
			_, _ = w.Write([]byte(`{
				"id":"gen_upstream_chat",
				"object":"chat.completion",
				"model":"real/upstream-chat",
				"provider":"InternalProvider",
				"generationId":"gen_internal",
				"system_fingerprint":"fp_internal",
				"service_tier":"provider-tier",
				"choices":[{"message":{"role":"assistant","content":"OK","provider_metadata":{"gateway":{"cost":"1","routing":{"finalProvider":"internal","providerRequestId":"request_internal"}}},"tool_calls":[{"id":"call_upstream","type":"function","function":{"name":"lookup","arguments":"{\\\"provider\\\":\\\"customer-provider\\\",\\\"model\\\":\\\"customer-model\\\",\\\"cost\\\":7}"}}]}}],
				"usage":{"prompt_tokens":1,"market_cost":0.1}
			}`))
		case "/messages":
			_, _ = w.Write([]byte(`{
				"id":"msg_upstream",
				"type":"message",
				"role":"assistant",
				"model":"real/upstream-message",
				"providerMetadata":{"generation_id":"gen_internal","provider_response_id":"response_internal"},
				"content":[{"type":"tool_use","id":"toolu_upstream","name":"lookup","input":{"provider":"customer-provider","model":"customer-model","cost":7}}],
				"usage":{"input_tokens":1,"output_tokens":1,"gatewayCost":0.2}
			}`))
		case "/responses":
			_, _ = w.Write([]byte(`{
				"id":"resp_upstream",
				"object":"response",
				"model":null,
				"provider_metadata":{"gateway":{"marketCost":0.3,"routing":{"originalModelId":"real/upstream-response","providerAttempts":[{"credentialType":"byok","providerRequestId":"request_internal"}]}}},
				"routing":{"fallbacksAvailable":[]},
				"generation_id":"gen_internal",
				"provider_request_id":"request_internal",
				"native_finish_reason":"stop",
				"output":[{"type":"message","id":"item_upstream","content":[{"type":"output_text","text":"OK"}]},{"type":"function_call","id":"call_upstream","name":"lookup","arguments":"{\\\"provider\\\":\\\"customer-provider\\\",\\\"model\\\":\\\"customer-model\\\",\\\"cost\\\":7}"}],
				"usage":{"input_tokens":1,"output_tokens":1,"cost_details":{"upstream_cost":0.3}}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	tests := []struct {
		name              string
		path              string
		body              string
		expectedID        string
		preservedToolData []string
	}{
		{name: "chat", path: "/v1/chat/completions", body: `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}]}`, expectedID: "gen_upstream_chat", preservedToolData: []string{"customer-provider", "customer-model", `\\\"cost\\\":7`}},
		{name: "messages", path: "/v1/messages", body: `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}],"max_tokens":16}`, expectedID: "msg_upstream", preservedToolData: []string{"customer-provider", "customer-model", `"cost":7`}},
		{name: "responses", path: "/v1/responses", body: `{"model":"kilo/public-model","input":"hi"}`, expectedID: "resp_upstream", preservedToolData: []string{"customer-provider", "customer-model", `\\\"cost\\\":7`}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, handler, tc.path, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}

			var payload any
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
			}
			assertSanitizedSuccessValue(t, payload, publicModel, false)
			if !strings.Contains(rec.Body.String(), `"id":"`+tc.expectedID+`"`) {
				t.Fatalf("standard response id was unexpectedly removed: %s", rec.Body.String())
			}
			for _, expected := range tc.preservedToolData {
				if !strings.Contains(rec.Body.String(), expected) {
					t.Fatalf("tool payload lost %q: %s", expected, rec.Body.String())
				}
			}

			for _, stripped := range []string{"Server", "Via", "CF-Ray", "NEL", "Report-To", "X-Upstream-Proxy", "X-Provider-Name", "X-Request-ID"} {
				if got := rec.Header().Get(stripped); got != "" {
					t.Fatalf("response header %s leaked: %q", stripped, got)
				}
			}
			if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
				t.Fatalf("content-type = %q", got)
			}
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("cache-control = %q", got)
			}
			if got := rec.Header().Get("Retry-After"); got != "7" {
				t.Fatalf("retry-after = %q", got)
			}
		})
	}
}

func TestProxySanitizesSuccessSSEAcrossProtocols(t *testing.T) {
	publicModel := "kilo/public-model"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Upstream-Proxy", "internal-proxy")
		flusher, _ := w.(http.Flusher)

		switch r.URL.Path {
		case "/chat/completions":
			_, _ = io.WriteString(w, "data: {\"id\":\"gen_upstream_chat\",\"model\":\"real/upstream-chat\",\"provider_metadata\":{\"gateway\":{\"cost\":1,\"generationId\":\"gen_internal\"}},\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"call_upstream\",\"function\":{\"arguments\":\"{\\\\\\\"provider\\\\\\\":\\\\\\\"customer-provider\\\\\\\",\\\\\\\"model\\\\\\\":\\\\\\\"customer-model\\\\\\\",\\\\\\\"cost\\\\\\\":7}\"}}]}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"gen_upstream_chat\",\"model\":null,\"usage\":{\"marketCost\":1,\"output_tokens\":1}}\n\n")
		case "/messages":
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_upstream\",\"model\":\"real/upstream-message\",\"providerMetadata\":{\"routing\":{\"providerRequestId\":\"request_internal\"}}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_upstream\",\"name\":\"lookup\",\"input\":{\"provider\":\"customer-provider\",\"model\":\"customer-model\",\"cost\":7}}}\n\n")
		case "/responses":
			_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_upstream\",\"model\":\"real/upstream-response\",\"provider_metadata\":{\"gateway\":{\"routing\":{\"finalProvider\":\"internal\"},\"providerResponseId\":\"response_internal\"}},\"output\":[{\"type\":\"function_call\",\"id\":\"call_upstream\",\"arguments\":\"{\\\\\\\"provider\\\\\\\":\\\\\\\"customer-provider\\\\\\\",\\\\\\\"model\\\\\\\":\\\\\\\"customer-model\\\\\\\",\\\\\\\"cost\\\\\\\":7}\"}]}}\n\n")
			_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_upstream\",\"model\":null,\"generation_id\":\"gen_internal\",\"usage\":{\"output_tokens\":1,\"surchargeCost\":1}}}\n\n")
		default:
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	tests := []struct {
		name              string
		path              string
		body              string
		expectedID        string
		preservedToolData []string
	}{
		{name: "chat", path: "/v1/chat/completions", body: `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, expectedID: "gen_upstream_chat", preservedToolData: []string{"customer-provider", "customer-model", `\\\"cost\\\":7`}},
		{name: "messages", path: "/v1/messages", body: `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"stream":true}`, expectedID: "msg_upstream", preservedToolData: []string{"customer-provider", "customer-model", `"cost":7`}},
		{name: "responses", path: "/v1/responses", body: `{"model":"kilo/public-model","input":"hi","stream":true}`, expectedID: "resp_upstream", preservedToolData: []string{"customer-provider", "customer-model", `\\\"cost\\\":7`}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, handler, tc.path, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("X-Upstream-Proxy"); got != "" {
				t.Fatalf("response header leaked: %q", got)
			}
			if !strings.Contains(rec.Body.String(), "data: [DONE]") {
				t.Fatalf("stream lost DONE marker: %s", rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"id":"`+tc.expectedID+`"`) {
				t.Fatalf("standard stream id was unexpectedly removed: %s", rec.Body.String())
			}
			for _, expected := range tc.preservedToolData {
				if !strings.Contains(rec.Body.String(), expected) {
					t.Fatalf("stream tool payload lost %q: %s", expected, rec.Body.String())
				}
			}

			payloads := decodeSSEDataPayloads(t, rec.Body.String())
			if len(payloads) == 0 {
				t.Fatalf("stream contained no JSON payloads: %s", rec.Body.String())
			}
			for _, payload := range payloads {
				assertSanitizedSuccessValue(t, payload, publicModel, false)
			}
		})
	}
}

func TestProxyFailsClosedForSSEErrorAndNonJSONData(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"InternalProvider failed at https://internal.example for real/upstream-model\"}}\n\n")
		_, _ = io.WriteString(w, "data: InternalProvider raw failure from https://internal.example\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	tests := []struct {
		name string
		path string
		body string
	}{
		{name: "chat", path: "/v1/chat/completions", body: `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}],"stream":true}`},
		{name: "messages", path: "/v1/messages", body: `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"stream":true}`},
		{name: "responses", path: "/v1/responses", body: `{"model":"kilo/public-model","input":"hi","stream":true}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := postJSON(t, handler, tc.path, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			for _, leaked := range []string{"InternalProvider", "internal.example", "real/upstream-model"} {
				if strings.Contains(rec.Body.String(), leaked) {
					t.Fatalf("SSE error leaked %q: %s", leaked, rec.Body.String())
				}
			}
			payloads := decodeSSEDataPayloads(t, rec.Body.String())
			if len(payloads) != 2 {
				t.Fatalf("sanitized SSE payloads = %d, want 2: %s", len(payloads), rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "upstream request failed") {
				t.Fatalf("sanitized SSE error missing generic message: %s", rec.Body.String())
			}
		})
	}
}

func TestProxySanitizesRateLimitModelLeak(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"Rate limit exceeded for poolside/laguna-xs-2.1:free via https://api.kilo.ai/path","code":429,"metadata":{"raw":"poolside/laguna-xs-2.1:free is temporarily rate-limited upstream","provider_name":"Poolside"}},"user_id":"org_2uwFc1szZKyZweUX7pX97kXQf2O"}`))
	}))
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/poolside/laguna-xs-2.1:free","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "poolside/laguna-xs-2.1:free") {
		t.Fatalf("rate-limit error leaked upstream model in body: %s", body)
	}
	if strings.Count(body, "<UPSTREAM_MODEL>") != 2 {
		t.Fatalf("sanitized model marker count mismatch in body: %s", body)
	}
	if strings.Contains(body, "Poolside") {
		t.Fatalf("rate-limit error leaked provider_name in body: %s", body)
	}
	if !strings.Contains(body, `"provider_name":"<redacted>"`) {
		t.Fatalf("provider_name was not redacted: %s", body)
	}
	for _, unchanged := range []string{"org_2uwFc1szZKyZweUX7pX97kXQf2O", "https://api.kilo.ai/path"} {
		if !strings.Contains(body, unchanged) {
			t.Fatalf("rate-limit sanitizer should not rewrite %q in body: %s", unchanged, body)
		}
	}
}

func TestProxyRedactsProviderNameInUpstreamErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Provider returned error ({\"raw\":\"{\\\"error\\\":{\\\"message\\\":\\\"Unrecognized chat message.\\\",\\\"type\\\":\\\"request_params_invalid\\\"}}\",\"provider_name\":\"StepFun\",\"is_byok\":true})","type":"request_params_invalid"},"provider_name":"StepFun"}`))
	}))
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/stepfun/step-3.7-flash:free","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "StepFun") {
		t.Fatalf("provider_name leaked in response body: %s", body)
	}
	if !strings.Contains(body, `"provider_name":"<redacted>"`) {
		t.Fatalf("provider_name was not redacted: %s", body)
	}
}

func TestProxyRedactsUpstreamModelInNonRateLimitErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"nvidia/nemotron-3-super-120b-a12b:free is not a valid model ID","code":400}}`))
	}))
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/nvidia/nemotron-3-super-120b-a12b:free","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "nvidia/nemotron-3-super-120b-a12b:free") {
		t.Fatalf("non-rate-limit error leaked upstream model in body: %s", body)
	}
	if strings.Contains(body, "kilo/nvidia/nemotron-3-super-120b-a12b:free") {
		t.Fatalf("non-rate-limit error leaked public model in body: %s", body)
	}
	if !strings.Contains(body, "<UPSTREAM_MODEL> is not a valid model ID") {
		t.Fatalf("non-rate-limit error did not redact model marker: %s", body)
	}
}

func TestProxyNormalizesSignInModelErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"You need to sign in to use this model.","code":401,"metadata":{"provider_name":"OpenAI","model":"openai/gpt-4.1:free"}}}`))
	}))
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/openai/gpt-4.1:free","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"message":"此模型正在维护很快恢复"`) {
		t.Fatalf("sign-in model error message was not normalized: %s", body)
	}
	if !strings.Contains(body, `"code":104`) {
		t.Fatalf("sign-in model error code was not normalized: %s", body)
	}
	for _, leaked := range []string{"You need to sign in to use this model.", "openai/gpt-4.1:free", "kilo/openai/gpt-4.1:free", "OpenAI"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("sign-in model error leaked %q in body: %s", leaked, body)
		}
	}
}

func TestProxySanitizesRateLimitIdentifierModelLeak(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"Rate limit exceeded: limit_rpm/cohere/north-mini-code-20260617/d3a18412-b833-40de-bc07-c4ba9a1dea53. High demand for cohere/north-mini-code:free on OpenRouter - limited to 15 requests per minute.","code":429}}`))
	}))
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/cohere/north-mini-code:free","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leaked := range []string{"cohere/north-mini-code:free", "limit_rpm/cohere/north-mini-code-20260617/d3a18412-b833-40de-bc07-c4ba9a1dea53", "cohere/north-mini-code-20260617"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("rate-limit error leaked model identifier %q in body: %s", leaked, body)
		}
	}
	for _, marker := range []string{"<RATE_LIMIT_ID>", "<UPSTREAM_MODEL>"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("sanitized error missing marker %q in body: %s", marker, body)
		}
	}
}

func TestProxySanitizesModelLeakOutsideRateLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Bad request for poolside/laguna-xs-2.1:free","code":400}}`))
	}))
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/poolside/laguna-xs-2.1:free","messages":[{"role":"user","content":"hi"}]}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "poolside/laguna-xs-2.1:free") {
		t.Fatalf("non-rate-limit error leaked upstream model in body: %s", body)
	}
	if !strings.Contains(body, "Bad request for <UPSTREAM_MODEL>") {
		t.Fatalf("non-rate-limit error did not redact model marker: %s", body)
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

func TestNvidiaSuperFreeUsesActual262KContextLimit(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	oldContent := "DROP_NVIDIA_SUPER_OLD " + strings.Repeat("old ", 160000)
	rec := postJSON(t, handler, "/v1/chat/completions", `{
		"model":"kilo/nvidia/nemotron-3-super-120b-a12b:free",
		"messages":[
			{"role":"system","content":"KEEP_NVIDIA_SUPER_SYSTEM"},
			{"role":"user","content":"`+oldContent+`"},
			{"role":"assistant","content":"`+strings.Repeat("assistant ", 160000)+`"},
			{"role":"user","content":"KEEP_NVIDIA_SUPER_LATEST"}
		],
		"tools":[{"type":"function","function":{"name":"probe","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],
		"max_tokens":64000
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	messages := (*calls)[0].Body["messages"].([]any)
	if !messagesContainText(messages, "KEEP_NVIDIA_SUPER_SYSTEM") {
		t.Fatalf("trimmed messages dropped system message; len=%d", len(messages))
	}
	if !messagesContainText(messages, "KEEP_NVIDIA_SUPER_LATEST") {
		t.Fatalf("trimmed messages dropped latest user; len=%d", len(messages))
	}
	if messagesContainText(messages, "DROP_NVIDIA_SUPER_OLD") {
		t.Fatalf("trimmed messages kept old nvidia super context; len=%d", len(messages))
	}
}

func TestLingFlashFreeUsesActual262KContextLimit(t *testing.T) {
	profile, ok := freeContextTrimProfiles["inclusionai/ling-3.0-flash:free"]
	if !ok {
		t.Fatal("ling flash context trim profile is missing")
	}
	wantProfile := contextTrimProfile{ContextLimit: 262144, SafetyMargin: 40000, MaxInputTokens: 200000}
	if profile != wantProfile {
		t.Fatalf("ling flash context trim profile = %#v, want %#v", profile, wantProfile)
	}

	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	oldContent := "DROP_LING_FLASH_OLD " + strings.Repeat("old ", 160000)
	rec := postJSON(t, handler, "/v1/chat/completions", `{
		"model":"kilo/inclusionai/ling-3.0-flash:free",
		"messages":[
			{"role":"system","content":"KEEP_LING_FLASH_SYSTEM"},
			{"role":"user","content":"`+oldContent+`"},
			{"role":"assistant","content":"`+strings.Repeat("assistant ", 160000)+`"},
			{"role":"user","content":"KEEP_LING_FLASH_LATEST"}
		],
		"tools":[{"type":"function","function":{"name":"probe","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],
		"max_tokens":64000
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	messages := (*calls)[0].Body["messages"].([]any)
	if !messagesContainText(messages, "KEEP_LING_FLASH_SYSTEM") {
		t.Fatalf("trimmed messages dropped system message; len=%d", len(messages))
	}
	if !messagesContainText(messages, "KEEP_LING_FLASH_LATEST") {
		t.Fatalf("trimmed messages dropped latest user; len=%d", len(messages))
	}
	if messagesContainText(messages, "DROP_LING_FLASH_OLD") {
		t.Fatalf("trimmed messages kept old ling flash context; len=%d", len(messages))
	}
}

func TestPoolsideXS21TruncatesOversizedLatestContentParts(t *testing.T) {
	upstream, calls := newRecordingUpstream(t)
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	rec := postJSON(t, handler, "/v1/chat/completions", `{
		"model":"kilo/poolside/laguna-xs-2.1:free",
		"messages":[
			{"role":"system","content":"KEEP_POOL_SYSTEM"},
			{"role":"user","content":[
				{"type":"text","text":"KEEP_POOL_HEAD `+strings.Repeat("middle ", 450000)+` KEEP_POOL_TAIL"}
			]}
		],
		"tools":[{"type":"function","function":{"name":"probe","parameters":{"type":"object","properties":{"q":{"type":"string"}}}}}],
		"max_tokens":64000
	}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	messages := (*calls)[0].Body["messages"].([]any)
	if !messagesContainText(messages, "KEEP_POOL_SYSTEM") {
		t.Fatalf("trimmed messages dropped system message; len=%d", len(messages))
	}
	if !messagesContainText(messages, "KEEP_POOL_HEAD") || !messagesContainText(messages, "KEEP_POOL_TAIL") {
		t.Fatalf("trimmed latest content did not preserve edges; len=%d", len(messages))
	}
	if tokens := contextEstimateItemsTokens(messages); tokens > 160000 {
		t.Fatalf("estimated messages tokens = %d, want under xs2.1 budget", tokens)
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

func decodeSSEDataPayloads(t *testing.T, body string) []any {
	t.Helper()
	var payloads []any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var payload any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("decode SSE data %q: %v", data, err)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

func assertSanitizedSuccessValue(t *testing.T, value any, publicModel string, opaque bool) {
	t.Helper()
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			normalized := normalizeTestJSONKey(key)
			if !opaque && isInternalSuccessTestKey(normalized) {
				t.Fatalf("success response leaked internal field %q in %#v", key, typed)
			}
			childOpaque := opaque || isOpaqueSuccessTestKey(normalized)
			if !childOpaque && normalized == "model" {
				model, ok := item.(string)
				if !ok || model != publicModel {
					t.Fatalf("model = %#v, want public request model %q", item, publicModel)
				}
			}
			assertSanitizedSuccessValue(t, item, publicModel, childOpaque)
		}
	case []any:
		for _, item := range typed {
			assertSanitizedSuccessValue(t, item, publicModel, opaque)
		}
	}
}

func normalizeTestJSONKey(key string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(key) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func isOpaqueSuccessTestKey(key string) bool {
	switch key {
	case "input", "arguments", "metadata", "parameters", "schema", "json", "parsed", "data":
		return true
	default:
		return false
	}
}

func isInternalSuccessTestKey(key string) bool {
	if strings.Contains(key, "cost") || strings.Contains(key, "provider") || strings.Contains(key, "routing") || strings.HasPrefix(key, "fallback") {
		return true
	}
	if strings.HasSuffix(key, "requestid") || strings.HasSuffix(key, "generationid") {
		return true
	}
	switch key {
	case "systemfingerprint", "servicetier", "nativefinishreason", "originalmodelid", "canonicalslug", "modelattempts", "providerattempts", "credentialtype", "planningreasoning", "isbyok":
		return true
	default:
		return false
	}
}
