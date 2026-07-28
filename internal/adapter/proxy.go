package adapter

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	publicModelPrefix            = "kilo/"
	signInModelErrorMessage      = "You need to sign in to use this model."
	modelMaintenanceErrorMessage = "此模型正在维护很快恢复"
)

type Handler struct {
	cfg    Config
	logger *requestLogger
}

func NewHandler(cfg Config) http.Handler {
	cfg = cfg.WithDefaults()
	var logger *requestLogger
	if cfg.RequestLogEnabled || strings.TrimSpace(cfg.RequestLogDir) != "" {
		logger = newRequestLogger(RequestLogConfig{
			Dir:       cfg.RequestLogDir,
			Retention: cfg.RequestLogRetention,
			BodyLimit: cfg.RequestLogBodyLimit,
		})
		if err := logger.cleanup(); err != nil {
			fmt.Printf("request_log_cleanup_error=%v\n", err)
		}
	}
	return &Handler{cfg: cfg, logger: logger}
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
		h.handleCountTokens(lw, r, start)
	case "/models":
		h.handleModels(lw, r, start)
	case "/chat/completions":
		h.handleProxy(lw, r, "/chat/completions", start)
	case "/responses":
		h.handleProxy(lw, r, "/responses", start)
	case "/messages":
		h.handleProxy(lw, r, "/messages", start)
	default:
		writeJSONError(lw, http.StatusNotFound, "not_found", "unsupported adapter path")
		h.recordRequest(start, r, "", nil, responseLogCapture{
			status:       http.StatusNotFound,
			bytesWritten: bytesWritten,
			adapterError: "unsupported adapter path",
		})
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

func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request, start time.Time) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		h.recordRequest(start, r, "", nil, responseLogCapture{
			status:       http.StatusMethodNotAllowed,
			adapterError: "method not allowed",
		})
		return
	}
	body, err := readLimited(r.Body, h.cfg.MaxBodyBytes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		h.recordRequest(start, r, "", nil, responseLogCapture{
			status:       http.StatusBadRequest,
			adapterError: err.Error(),
		})
		return
	}
	payload := map[string]int{"input_tokens": approximateTokens(body)}
	writeJSON(w, http.StatusOK, payload)
	h.recordRequest(start, r, "", body, responseLogCapture{
		status:      http.StatusOK,
		contentType: "application/json",
		body:        []byte(fmt.Sprintf(`{"input_tokens":%d}`, payload["input_tokens"])),
	})
}

func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request, start time.Time) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		h.recordRequest(start, r, "/models", nil, responseLogCapture{
			status:       http.StatusMethodNotAllowed,
			adapterError: "method not allowed",
		})
		return
	}

	resp, err := h.forward(r, "/models", nil)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "upstream_error", "upstream request failed")
		h.recordRequest(start, r, "/models", nil, responseLogCapture{
			status:        http.StatusBadGateway,
			upstreamError: err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "upstream_error", "upstream response read failed")
		h.recordRequest(start, r, "/models", nil, responseLogCapture{
			status:        http.StatusBadGateway,
			upstreamError: err.Error(),
		})
		return
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body = prefixModelsResponse(body)
	} else {
		body = redactProviderName(body)
	}

	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	capture := newCaptureBuffer(h.cfg.RequestLogBodyLimit)
	_, _ = io.Copy(w, io.TeeReader(bytes.NewReader(body), capture))
	h.recordRequest(start, r, "/models", nil, responseLogCapture{
		status:        resp.StatusCode,
		bytesWritten:  capture.Total(),
		contentType:   resp.Header.Get("Content-Type"),
		body:          capture.Bytes(),
		bodyTruncated: capture.Truncated(),
	})
}

func (h *Handler) handleProxy(w http.ResponseWriter, r *http.Request, upstreamPath string, start time.Time) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		h.recordRequest(start, r, upstreamPath, nil, responseLogCapture{
			status:       http.StatusMethodNotAllowed,
			adapterError: "method not allowed",
		})
		return
	}

	body, err := readLimited(r.Body, h.cfg.MaxBodyBytes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		h.recordRequest(start, r, upstreamPath, nil, responseLogCapture{
			status:       http.StatusBadRequest,
			adapterError: err.Error(),
		})
		return
	}

	body, err = validateJSONBody(body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
			status:       http.StatusBadRequest,
			adapterError: err.Error(),
		})
		return
	}
	publicRequestModel, err := validatePublicRequestModel(body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
			status:       http.StatusBadRequest,
			adapterError: err.Error(),
		})
		return
	}

	body, err = stripPublicModelPrefix(body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
			status:       http.StatusBadRequest,
			adapterError: err.Error(),
		})
		return
	}

	body, responseFormatDropped, err := dropLingResponseFormat(body, upstreamPath)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
			status:       http.StatusBadRequest,
			adapterError: err.Error(),
		})
		return
	}
	if responseFormatDropped {
		fmt.Printf("kilo_response_format_drop path=%s model=%s\n", upstreamPath, lingFlashFreeModel)
	}

	body, jsonModeDowngraded, err := downgradeFreeModelJSONMode(body, upstreamPath)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
			status:       http.StatusBadRequest,
			adapterError: err.Error(),
		})
		return
	}
	if jsonModeDowngraded {
		fmt.Printf("kilo_json_mode_downgrade path=%s model=%s mode=json_object\n", upstreamPath, stepfunFlashFreeModel)
	}

	body, trimStats, err := trimFreeModelContext(body, upstreamPath)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
			status:       http.StatusBadRequest,
			adapterError: err.Error(),
		})
		return
	}
	if trimStats.changed() {
		fmt.Printf("kilo_context_trim path=%s model=%s trimmed_items=%d truncated_items=%d output_clamped=%t\n",
			upstreamPath, trimStats.Model, trimStats.TrimmedItems, trimStats.TruncatedItems, trimStats.OutputClamped)
	}

	resp, err := h.forward(r, upstreamPath, bytes.NewReader(body))
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "upstream_error", "upstream request failed")
		h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
			status:        http.StatusBadGateway,
			upstreamError: err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Set("Cache-Control", "no-store")
	if resp.StatusCode == http.StatusTooManyRequests {
		responseBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			writeJSONError(w, http.StatusBadGateway, "upstream_error", "upstream response read failed")
			h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
				status:        http.StatusBadGateway,
				upstreamError: readErr.Error(),
			})
			return
		}
		sanitized := sanitizeUpstreamErrorBody(body, responseBody)
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(sanitized)
		h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
			status:      resp.StatusCode,
			contentType: resp.Header.Get("Content-Type"),
			body:        sanitized,
		})
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			writeJSONError(w, http.StatusBadGateway, "upstream_error", "upstream response read failed")
			h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
				status:        http.StatusBadGateway,
				upstreamError: readErr.Error(),
			})
			return
		}
		sanitized := sanitizeUpstreamErrorBody(body, responseBody)
		w.Header().Del("Content-Length")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(sanitized)
		h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
			status:      resp.StatusCode,
			contentType: resp.Header.Get("Content-Type"),
			body:        sanitized,
		})
		return
	}

	capture := newCaptureBuffer(h.cfg.RequestLogBodyLimit)
	w.Header().Del("Content-Length")
	responseStatus := resp.StatusCode
	if isEventStreamResponse(resp.Header) {
		w.WriteHeader(responseStatus)
		copyEventStreamWithNormalizeAndCapture(w, resp.Body, capture, publicRequestModel, upstreamPath)
	} else {
		responseBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			responseStatus = http.StatusBadGateway
			responseBody = publicUpstreamErrorBody(upstreamPath)
			w.WriteHeader(responseStatus)
			_, _ = io.Copy(w, io.TeeReader(bytes.NewReader(responseBody), capture))
			fmt.Printf("upstream_success_read_error=%v\n", readErr)
			h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
				status:        responseStatus,
				bytesWritten:  capture.Total(),
				contentType:   "application/json",
				body:          capture.Bytes(),
				bodyTruncated: capture.Truncated(),
				upstreamError: readErr.Error(),
			})
			return
		}
		var validSuccess bool
		responseBody, validSuccess = normalizeSuccessResponseBodyChecked(responseBody, publicRequestModel, upstreamPath, false)
		if !validSuccess {
			responseStatus = http.StatusBadGateway
		}
		w.WriteHeader(responseStatus)
		_, _ = io.Copy(w, io.TeeReader(bytes.NewReader(responseBody), capture))
	}
	h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
		status:        responseStatus,
		bytesWritten:  capture.Total(),
		contentType:   resp.Header.Get("Content-Type"),
		body:          capture.Bytes(),
		bodyTruncated: capture.Truncated(),
	})
}

type responseLogCapture struct {
	status        int
	bytesWritten  int64
	contentType   string
	body          []byte
	bodyTruncated bool
	adapterError  string
	upstreamError string
}

func (h *Handler) recordRequest(start time.Time, r *http.Request, upstreamPath string, body []byte, resp responseLogCapture) {
	if h.logger == nil {
		return
	}
	if resp.status == 0 {
		resp.status = http.StatusOK
	}
	if resp.bytesWritten == 0 {
		resp.bytesWritten = int64(len(resp.body))
	}
	err := h.logger.record(loggableRequest{
		Method:       r.Method,
		Path:         r.URL.Path,
		Query:        r.URL.RawQuery,
		UpstreamPath: upstreamPath,
		RemoteAddr:   r.RemoteAddr,
		UserAgent:    r.UserAgent(),
		ContentType:  r.Header.Get("Content-Type"),
		Body:         body,
	}, loggableResponse{
		Status:        resp.status,
		Duration:      time.Since(start),
		BytesWritten:  resp.bytesWritten,
		ContentType:   resp.contentType,
		Body:          resp.body,
		Truncated:     resp.bodyTruncated,
		AdapterError:  resp.adapterError,
		UpstreamError: resp.upstreamError,
	})
	if err != nil {
		fmt.Printf("request_log_error=%v\n", err)
	}
}

var (
	rateLimitIdentifierPattern      = regexp.MustCompile(`\blimit_[A-Za-z0-9_-]+(?:\\?/[A-Za-z0-9_.:-]+){2,4}\b`)
	providerNameFieldPattern        = regexp.MustCompile(`("provider_name"\s*:\s*")[^"]*(")`)
	escapedProviderNameFieldPattern = regexp.MustCompile(`(\\"provider_name\\"\s*:\s*\\")[^\\"\\\\]*(\\")`)
)

func sanitizeRateLimitModelLeak(requestBody []byte, responseBody []byte) []byte {
	model := strings.TrimSpace(extractModel(requestBody))
	if model == "" || len(responseBody) == 0 {
		return responseBody
	}
	sanitized := rateLimitIdentifierPattern.ReplaceAll(responseBody, []byte("<RATE_LIMIT_ID>"))
	sanitized = bytes.ReplaceAll(sanitized, []byte(prefixPublicModel(model)), []byte("<UPSTREAM_MODEL>"))
	sanitized = bytes.ReplaceAll(sanitized, []byte(model), []byte("<UPSTREAM_MODEL>"))
	escapedModel := strings.ReplaceAll(model, "/", `\/`)
	if escapedModel != model {
		sanitized = bytes.ReplaceAll(sanitized, []byte(escapedModel), []byte("<UPSTREAM_MODEL>"))
	}
	escapedPublicModel := strings.ReplaceAll(prefixPublicModel(model), "/", `\/`)
	if escapedPublicModel != prefixPublicModel(model) {
		sanitized = bytes.ReplaceAll(sanitized, []byte(escapedPublicModel), []byte("<UPSTREAM_MODEL>"))
	}
	return sanitized
}

func sanitizeUpstreamErrorBody(requestBody []byte, responseBody []byte) []byte {
	sanitized := sanitizeRateLimitModelLeak(requestBody, responseBody)
	sanitized = redactProviderName(sanitized)
	return normalizeSignInModelError(sanitized)
}

func normalizeSignInModelError(body []byte) []byte {
	if !bytes.Contains(body, []byte(signInModelErrorMessage)) {
		return body
	}
	return []byte(`{"error":{"message":"` + modelMaintenanceErrorMessage + `","type":"model_maintenance","code":104}}`)
}

func redactProviderName(body []byte) []byte {
	redacted := providerNameFieldPattern.ReplaceAll(body, []byte(`${1}<redacted>${2}`))
	redacted = escapedProviderNameFieldPattern.ReplaceAll(redacted, []byte(`${1}<redacted>${2}`))
	return redacted
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

func stripPublicModelPrefix(body []byte) ([]byte, error) {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %w", err)
	}
	if object, ok := payload.(map[string]any); ok {
		if raw, ok := object["model"].(string); ok {
			object["model"] = strings.TrimPrefix(strings.TrimSpace(raw), publicModelPrefix)
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func prefixModelsResponse(body []byte) []byte {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	if object, ok := payload.(map[string]any); ok {
		prefixModelsData(object["data"])
	} else {
		prefixModelsData(payload)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

func prefixModelsData(value any) {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if model, ok := item.(map[string]any); ok {
				prefixModelListObject(model)
			}
		}
	}
}

func prefixModelListObject(model map[string]any) {
	for _, key := range []string{"id", "canonical_slug"} {
		if raw, ok := model[key].(string); ok {
			model[key] = prefixPublicModel(raw)
		}
	}
	if autoRouting, ok := model["autoRouting"].(map[string]any); ok {
		prefixStringArrayField(autoRouting, "models")
	}
}

func prefixStringArrayField(object map[string]any, key string) {
	values, ok := object[key].([]any)
	if !ok {
		return
	}
	for i, value := range values {
		if raw, ok := value.(string); ok {
			values[i] = prefixPublicModel(raw)
		}
	}
}

func prefixPublicModel(model string) string {
	if strings.HasPrefix(model, publicModelPrefix) {
		return model
	}
	return publicModelPrefix + model
}

func validatePublicRequestModel(body []byte) (string, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("invalid JSON body: %w", err)
	}
	raw, ok := payload["model"]
	if !ok {
		return "", errors.New("model must be a non-empty string")
	}
	model, ok := raw.(string)
	if !ok || strings.TrimSpace(model) == "" {
		return "", errors.New("model must be a non-empty string")
	}
	model = strings.TrimPrefix(strings.TrimSpace(model), publicModelPrefix)
	if model == "" {
		return "", errors.New("model must be a non-empty string")
	}
	return prefixPublicModel(model), nil
}

type successSanitizeContext struct {
	envelope    bool
	ensureModel bool
	usage       bool
}

func normalizeSuccessResponseBody(body []byte, publicModel string, upstreamPath string, streaming bool) []byte {
	normalized, _ := normalizeSuccessResponseBodyChecked(body, publicModel, upstreamPath, streaming)
	return normalized
}

func normalizeSuccessResponseBodyChecked(body []byte, publicModel string, upstreamPath string, streaming bool) ([]byte, bool) {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return publicUpstreamErrorBody(upstreamPath), false
	}
	if successPayloadHasError(payload) {
		return publicUpstreamErrorBody(upstreamPath), false
	}
	normalizeSuccessPayload(payload, publicModel, upstreamPath, successRootContext(upstreamPath, streaming))
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body, true
	}
	return encoded, true
}

func successRootContext(upstreamPath string, streaming bool) successSanitizeContext {
	context := successSanitizeContext{envelope: true}
	if !streaming || upstreamPath == "/chat/completions" {
		context.ensureModel = true
	}
	return context
}

func normalizeSuccessPayload(value any, publicModel string, upstreamPath string, context successSanitizeContext) {
	switch typed := value.(type) {
	case map[string]any:
		hasModel := false
		for key, item := range typed {
			normalizedKey := normalizeSuccessFieldName(key)
			if normalizedKey == "providermetadata" || ((context.envelope || context.usage) && shouldRemoveSuccessField(normalizedKey)) {
				delete(typed, key)
				continue
			}
			if context.envelope && normalizedKey == "model" {
				typed[key] = publicModel
				hasModel = true
				continue
			}
			if isOpaqueSuccessField(normalizedKey) {
				continue
			}
			normalizeSuccessPayload(item, publicModel, upstreamPath, successChildContext(normalizedKey, upstreamPath, context))
		}
		if context.ensureModel && !hasModel && publicModel != "" {
			typed["model"] = publicModel
		}
	case []any:
		for _, item := range typed {
			normalizeSuccessPayload(item, publicModel, upstreamPath, context)
		}
	}
}

func successChildContext(key string, upstreamPath string, parent successSanitizeContext) successSanitizeContext {
	if parent.usage {
		return successSanitizeContext{usage: true}
	}
	switch key {
	case "usage", "usagedetails", "tokenusage":
		return successSanitizeContext{usage: true}
	case "response":
		return successSanitizeContext{envelope: true, ensureModel: true}
	case "message":
		return successSanitizeContext{envelope: true, ensureModel: upstreamPath == "/messages"}
	case "choices", "choice", "delta", "output", "item", "content", "contentblock":
		return successSanitizeContext{envelope: true}
	default:
		return successSanitizeContext{envelope: parent.envelope}
	}
}

func successPayloadHasError(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if rawType, ok := typed["type"].(string); ok && strings.EqualFold(strings.TrimSpace(rawType), "error") {
			return true
		}
		for key, item := range typed {
			normalizedKey := normalizeSuccessFieldName(key)
			if normalizedKey == "error" && meaningfulErrorValue(item) {
				return true
			}
			if isOpaqueSuccessField(normalizedKey) {
				continue
			}
			if successPayloadHasError(item) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if successPayloadHasError(item) {
				return true
			}
		}
	}
	return false
}

func meaningfulErrorValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case map[string]any:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	case bool:
		return typed
	default:
		return true
	}
}

func publicUpstreamErrorBody(upstreamPath string) []byte {
	if upstreamPath == "/messages" {
		return []byte(`{"type":"error","error":{"type":"api_error","message":"upstream request failed"}}`)
	}
	return []byte(`{"error":{"message":"upstream request failed","type":"upstream_error","code":"upstream_error"}}`)
}

func normalizeSuccessFieldName(key string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(key) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func isOpaqueSuccessField(key string) bool {
	switch key {
	case "input", "arguments", "metadata", "parameters", "schema", "json", "parsed", "data":
		return true
	default:
		return false
	}
}

func shouldRemoveSuccessField(key string) bool {
	if strings.Contains(key, "cost") || strings.Contains(key, "provider") || strings.Contains(key, "routing") || strings.HasPrefix(key, "fallback") {
		return true
	}
	if strings.HasSuffix(key, "requestid") || strings.HasSuffix(key, "generationid") {
		return true
	}
	switch key {
	case "systemfingerprint", "servicetier", "nativefinishreason", "originalmodelid", "canonicalslug", "modelattempts", "credentialtype", "planningreasoning", "isbyok", "userid", "accountid", "organizationid", "sessionid", "chatid":
		return true
	default:
		return false
	}
}

func isEventStreamResponse(header http.Header) bool {
	return strings.Contains(strings.ToLower(header.Get("Content-Type")), "text/event-stream")
}

func copyEventStreamWithNormalizeAndCapture(w http.ResponseWriter, r io.Reader, capture *captureBuffer, publicModel string, upstreamPath string) {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			chunk := normalizeEventStreamLine(line, publicModel, upstreamPath)
			if capture != nil {
				_, _ = capture.Write(chunk)
			}
			_, _ = w.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func normalizeEventStreamLine(line []byte, publicModel string, upstreamPath string) []byte {
	trimmed := bytes.TrimRight(line, "\r\n")
	lineEnding := line[len(trimmed):]
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line
	}
	data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return line
	}
	normalized := normalizeSuccessResponseBody(data, publicModel, upstreamPath, true)
	output := make([]byte, 0, len("data: ")+len(normalized)+len(lineEnding))
	output = append(output, "data: "...)
	output = append(output, normalized...)
	output = append(output, lineEnding...)
	return output
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
	req.Header.Set("Accept-Encoding", "identity")
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
		"accept-encoding",
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
		if !shouldForwardResponseHeader(key) {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func shouldForwardResponseHeader(key string) bool {
	switch strings.ToLower(key) {
	case "content-type",
		"cache-control",
		"content-language",
		"retry-after",
		"vary",
		"access-control-allow-origin",
		"access-control-allow-methods",
		"access-control-allow-headers",
		"access-control-expose-headers":
		return true
	default:
		return false
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
