package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

type Handler struct {
	cfg Config
}

func NewHandler(cfg Config) http.Handler {
	return &Handler{cfg: cfg.WithDefaults()}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := http.StatusOK
	bytesWritten := int64(0)
	lw := &loggingResponseWriter{ResponseWriter: w, status: &status, bytesWritten: &bytesWritten}

	defer func() {
		if r.URL.Path != "/healthz" {
			fmt.Printf("method=%s path=%s status=%d bytes=%d duration_ms=%d\n",
				r.Method, r.URL.Path, status, bytesWritten, time.Since(start).Milliseconds())
		}
	}()

	switch normalizePath(r.URL.Path) {
	case "/healthz":
		h.handleHealth(lw, r)
	case "/messages/count_tokens":
		h.handleCountTokens(lw, r)
	case "/models":
		h.handleModels(lw, r)
	case "/chat/completions":
		h.handleProxy(lw, r, "/chat/completions")
	case "/responses":
		h.handleProxy(lw, r, "/responses")
	case "/messages":
		h.handleProxy(lw, r, "/messages")
	default:
		writeJSONError(lw, http.StatusNotFound, "not_found", "unsupported adapter path")
	}
}

func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	body, err := readLimited(r.Body, h.cfg.MaxBodyBytes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	payload := map[string]int{"input_tokens": approximateTokens(body)}
	writeJSON(w, http.StatusOK, payload)
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	resp, err := h.forward(r, "/models", nil)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "upstream_error", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	copyStreaming(w, resp.Body)
}

func (h *Handler) handleProxy(w http.ResponseWriter, r *http.Request, upstreamPath string) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}

	body, err := readLimited(r.Body, h.cfg.MaxBodyBytes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	body, err = validateJSONBody(body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	resp, err := h.forward(r, upstreamPath, bytes.NewReader(body))
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "upstream_error", "upstream request failed")
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	copyStreaming(w, resp.Body)
}

func validateJSONBody(body []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, errors.New("empty JSON body")
	}
	var payload any
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	return trimmed, nil
}

func (h *Handler) forward(original *http.Request, upstreamPath string, body io.Reader) (*http.Response, error) {
	target := h.cfg.UpstreamBaseURL + upstreamPath
	if original.URL.RawQuery != "" {
		target += "?" + original.URL.RawQuery
	}

	method := original.Method
	if body != nil {
		method = http.MethodPost
	}

	req, err := http.NewRequestWithContext(original.Context(), method, target, body)
	if err != nil {
		return nil, err
	}
	copyRequestHeaders(req.Header, original.Header)
	req.Header.Set("Connection", "close")
	req.Header.Set("User-Agent", "kilo-newapi-adapter/0.1")
	if body != nil && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.cfg.UpstreamAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+h.cfg.UpstreamAPIKey)
	}

	return h.cfg.HTTPClient.Do(req)
}

func normalizePath(path string) string {
	if path == "" {
		return "/"
	}
	if strings.HasPrefix(path, "/v1/") {
		return strings.TrimPrefix(path, "/v1")
	}
	if path == "/v1" {
		return "/"
	}
	return path
}

func readLimited(body io.ReadCloser, max int64) ([]byte, error) {
	if body == nil {
		return nil, errors.New("missing request body")
	}
	defer body.Close()

	limited := io.LimitReader(body, max+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("request body exceeds %d bytes", max)
	}
	return data, nil
}

func copyRequestHeaders(dst, src http.Header) {
	for key, values := range src {
		canonical := http.CanonicalHeaderKey(key)
		if shouldStripRequestHeader(canonical) {
			continue
		}
		for _, value := range values {
			dst.Add(canonical, value)
		}
	}
}

func shouldStripRequestHeader(key string) bool {
	switch strings.ToLower(key) {
	case "authorization",
		"cookie",
		"proxy-authorization",
		"x-provider-base-url",
		"x-provider-baseurl",
		"x-base-url",
		"x-upstream-url",
		"x-upstream-base-url",
		"host",
		"content-length":
		return true
	default:
		return false
	}
}

func copyResponseHeaders(dst, src http.Header) {
	for key, values := range src {
		if strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copyStreaming(w http.ResponseWriter, r io.Reader) {
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32*1024)
	for {
		n, err := r.Read(buffer)
		if n > 0 {
			_, _ = w.Write(buffer[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func approximateTokens(body []byte) int {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return max(1, utf8.RuneCount(body)/4)
	}

	runes := countTextRunes(payload)
	if runes == 0 {
		runes = utf8.RuneCount(body)
	}
	return max(1, (runes+3)/4)
}

func countTextRunes(value any) int {
	switch typed := value.(type) {
	case string:
		return utf8.RuneCountInString(typed)
	case []any:
		total := 0
		for _, item := range typed {
			total += countTextRunes(item)
		}
		return total
	case map[string]any:
		total := 0
		for key, item := range typed {
			switch key {
			case "content", "text", "input", "system":
				total += countTextRunes(item)
			case "messages":
				total += countTextRunes(item)
			}
		}
		return total
	default:
		return 0
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    code,
			"code":    code,
		},
	})
}

type loggingResponseWriter struct {
	http.ResponseWriter
	status       *int
	bytesWritten *int64
}

func (w *loggingResponseWriter) WriteHeader(status int) {
	*w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *loggingResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	*w.bytesWritten += int64(n)
	return n, err
}
