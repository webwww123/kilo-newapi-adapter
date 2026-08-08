package adapter

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStepfunJSONCompatReturnsPublicReviewError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnavailableForLegalReasons)
		_, _ = io.WriteString(w, `{"error":{"message":"private upstream review detail","type":"censorship_blocked"}}`)
	}))
	defer upstream.Close()

	handler := newTestHandler(upstream.URL)
	rec := postJSON(t, handler, "/v1/chat/completions", `{
		"model":"kilo/stepfun/step-3.7-flash:free",
		"messages":[{"role":"user","content":"reply with a JSON object"}],
		"response_format":{"type":"json_object"}
	}`)

	if rec.Code != publicReviewStatus {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, publicReviewStatus, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, publicReviewErrorMessage) || !strings.Contains(body, `"code":450`) {
		t.Fatalf("review response missing public status/message: %s", body)
	}
	for _, leaked := range []string{"private upstream review detail", "censorship_blocked"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("review response leaked %q: %s", leaked, body)
		}
	}
}
