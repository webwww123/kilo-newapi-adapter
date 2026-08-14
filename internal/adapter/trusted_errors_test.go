package adapter

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTrustedNewAPIErrorsPreserveDiagnosticsAndScrubSecrets(t *testing.T) {
	tests := []struct {
		path           string
		upstreamStatus int
		wantStatus     int
	}{
		{path: "/v1/chat/completions", upstreamStatus: http.StatusUnauthorized, wantStatus: http.StatusBadRequest},
		{path: "/v1/messages", upstreamStatus: http.StatusTooManyRequests, wantStatus: http.StatusTooManyRequests},
		{path: "/v1/responses", upstreamStatus: http.StatusServiceUnavailable, wantStatus: http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.upstreamStatus)
				_, _ = io.WriteString(w, `{"error":{"message":"InternalProvider rejected real/upstream-model at https://internal.example/v1?token=secret-token","type":"provider_error","code":"route_failed","metadata":{"provider":"InternalProvider","route":"pool-a","api_key":"sk-live-secret123","raw":"Authorization: Bearer secret-token"}}}`)
			}))
			defer upstream.Close()

			handler := NewHandler(Config{UpstreamBaseURL: upstream.URL, TrustedNewAPIErrors: true})
			requestBody := `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}],"input":"hi","max_tokens":16}`
			rec := postJSON(t, handler, tc.path, requestBody)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			body := rec.Body.String()
			for _, want := range []string{"InternalProvider", "real/upstream-model", "internal.example", "provider_error", "route_failed", "pool-a"} {
				if !strings.Contains(body, want) {
					t.Fatalf("trusted diagnostic lost %q: %s", want, body)
				}
			}
			for _, forbidden := range []string{"secret-token", "sk-live-secret123", `"api_key":"sk-live-secret123"`, "Bearer secret-token"} {
				if strings.Contains(body, forbidden) {
					t.Fatalf("trusted diagnostic leaked %q: %s", forbidden, body)
				}
			}
			var payload any
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatalf("trusted diagnostic is invalid JSON: %v body=%s", err, body)
			}
		})
	}
}

func TestTrustedNewAPIErrorsDoNotAffectHTTP200ErrorEnvelopes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"error":{"message":"PRIVATE_HTTP_200_ERROR"}}`)
	}))
	defer upstream.Close()

	handler := NewHandler(Config{UpstreamBaseURL: upstream.URL, TrustedNewAPIErrors: true})
	rec := postJSON(t, handler, "/v1/chat/completions", `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "PRIVATE_HTTP_200_ERROR") || !strings.Contains(rec.Body.String(), publicServerErrorMessage) {
		t.Fatalf("HTTP 200 error envelope bypassed existing protection: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestConfigFromEnvEnablesTrustedNewAPIErrors(t *testing.T) {
	t.Setenv("TRUSTED_NEWAPI_ERROR_DIAGNOSTICS", "true")
	if !ConfigFromEnv().TrustedNewAPIErrors {
		t.Fatal("trusted NewAPI diagnostics were not enabled from the environment")
	}
}
