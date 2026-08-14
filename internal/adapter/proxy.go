package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	publicModelPrefix           = "kilo/"
	publicReviewStatus          = 450
	publicClientErrorMessage    = "400 请求无法处理，请检查请求参数或稍后重试。"
	publicRateLimitErrorMessage = "429 请求过于频繁，请稍后重试。"
	publicReviewErrorMessage    = "触发审查，请重试"
	publicServerErrorMessage    = "500 请求无法处理，请检查请求参数或稍后重试。"
	lingTiny429RetryBudget      = 30 * time.Second
	structuredJSONRetryBudget   = 5 * time.Second
	kiloAutoRetryBudget         = 15 * time.Second
)

var lingTiny429RetryBackoff = [...]time.Duration{3 * time.Second, 7 * time.Second, 20 * time.Second}
var structuredJSONRetryBackoff = [...]time.Duration{time.Second}
var kiloAutoRetryBackoff = [...]time.Duration{time.Second, 3 * time.Second}

type Handler struct {
	cfg         Config
	logger      *requestLogger
	retryWait   func(context.Context, time.Duration) error
	retryJitter func(time.Duration) time.Duration
}

func NewHandler(cfg Config) http.Handler {
	base := newBaseHandler(cfg)
	jsonCompat := newLingJSONCompatHandler(base, base.cfg.MaxBodyBytes)
	if !base.cfg.KiloAutoReliabilityEnabled {
		return jsonCompat
	}
	return newKiloAutoReliabilityHandler(jsonCompat, base.cfg.MaxBodyBytes)
}

func newBaseHandler(cfg Config) *Handler {
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
	return &Handler{
		cfg:         cfg,
		logger:      logger,
		retryWait:   waitForRetry,
		retryJitter: defaultRetryJitter,
	}
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
	case "/embeddings":
		h.handleProxy(lw, r, "/embeddings", start)
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
	copyResponseHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		body = prefixModelsResponse(body)
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
		return
	}

	rawCapture := newCaptureBuffer(h.cfg.RequestLogBodyLimit)
	_, _ = rawCapture.Write(body)
	writeUpstreamErrorResponse(w, resp.StatusCode, "/models", body, h.cfg.TrustedNewAPIErrors)
	h.recordRequest(start, r, "/models", nil, responseLogCapture{
		status:        resp.StatusCode,
		bytesWritten:  rawCapture.Total(),
		contentType:   resp.Header.Get("Content-Type"),
		body:          rawCapture.Bytes(),
		bodyTruncated: rawCapture.Truncated(),
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

	body, preloaded := lingJSONPreloadedBody(r)
	var err error
	if !preloaded {
		body, err = readLimited(r.Body, h.cfg.MaxBodyBytes)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			h.recordRequest(start, r, upstreamPath, nil, responseLogCapture{
				status:       http.StatusBadRequest,
				adapterError: err.Error(),
			})
			return
		}
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

	body, lingStats, lingRejection, err := normalizeLingRequest(body, upstreamPath)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
			status:       http.StatusBadRequest,
			adapterError: err.Error(),
		})
		return
	}
	if lingRejection != nil {
		writeJSONError(w, http.StatusBadRequest, lingRejection.Code, lingRejection.Message)
		h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
			status:       http.StatusBadRequest,
			adapterError: lingRejection.Code,
		})
		return
	}
	if upstreamPath == "/embeddings" {
		writeJSONError(w, http.StatusNotFound, "not_found", "unsupported adapter path")
		h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
			status:       http.StatusNotFound,
			adapterError: "unsupported adapter path",
		})
		return
	}
	if lingStats.Changed {
		fmt.Printf("kilo_ling_compat path=%s reasoning_dropped=%t structured_output_dropped=%t oversized_output_dropped=%t\n",
			upstreamPath, lingStats.ReasoningDropped, lingStats.StructuredOutputDropped, lingStats.OversizedOutputDropped)
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
	stillTooLarge, err := lingRequestStillExceedsContextLimit(body, upstreamPath)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
			status:       http.StatusBadRequest,
			adapterError: err.Error(),
		})
		return
	}
	if stillTooLarge {
		rejection := lingContextOverflowRejection()
		writeJSONError(w, http.StatusBadRequest, rejection.Code, rejection.Message)
		h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
			status:       http.StatusBadRequest,
			adapterError: rejection.Code,
		})
		return
	}

	resp, err := h.forwardWithModelRetry(r, upstreamPath, body, publicRequestModel)
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
		rawCapture := newCaptureBuffer(h.cfg.RequestLogBodyLimit)
		_, _ = rawCapture.Write(responseBody)
		writeUpstreamErrorResponse(w, resp.StatusCode, upstreamPath, responseBody, h.cfg.TrustedNewAPIErrors)
		h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
			status:        resp.StatusCode,
			bytesWritten:  rawCapture.Total(),
			contentType:   resp.Header.Get("Content-Type"),
			body:          rawCapture.Bytes(),
			bodyTruncated: rawCapture.Truncated(),
		})
		return
	}

	logCapture := newCaptureBuffer(h.cfg.RequestLogBodyLimit)
	w.Header().Del("Content-Length")
	responseStatus := resp.StatusCode
	if isEventStreamResponse(resp.Header) {
		w.WriteHeader(responseStatus)
		audit := copyEventStreamWithNormalizeAndCapture(w, resp.Body, logCapture, publicRequestModel, upstreamPath)
		if audit.HadError {
			responseStatus = http.StatusInternalServerError
		}
		if publicRequestModel == nemotronLightningJSONPublicModel {
			fmt.Printf("kilo_stream_audit model=nemotron_lightning http_status=%d events=%d parse_errors=%d error_events=%d visible_deltas=%d tool_deltas=%d completion_tokens=%d done=%t suspected_soft_error=%t\n",
				resp.StatusCode, audit.Events, audit.ParseErrors, audit.ErrorEvents, audit.VisibleDeltas, audit.ToolDeltas, audit.CompletionTokens, audit.Done, audit.SuspectedSoftError())
		}
	} else {
		responseBody, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			responseStatus = http.StatusInternalServerError
			responseBody = publicUpstreamErrorBody(upstreamPath)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(responseStatus)
			_, _ = io.Copy(w, io.TeeReader(bytes.NewReader(responseBody), logCapture))
			fmt.Printf("upstream_success_read_error=%v\n", readErr)
			h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
				status:        responseStatus,
				bytesWritten:  logCapture.Total(),
				contentType:   "application/json",
				body:          logCapture.Bytes(),
				bodyTruncated: logCapture.Truncated(),
				upstreamError: readErr.Error(),
			})
			return
		}
		rawResponseBody := responseBody
		var validSuccess bool
		responseBody, validSuccess = normalizeSuccessResponseBodyChecked(responseBody, publicRequestModel, upstreamPath, false)
		if !validSuccess {
			responseStatus = http.StatusInternalServerError
			w.Header().Set("Content-Type", "application/json")
			_, _ = logCapture.Write(rawResponseBody)
		}
		w.WriteHeader(responseStatus)
		if validSuccess {
			_, _ = io.Copy(w, io.TeeReader(bytes.NewReader(responseBody), logCapture))
		} else {
			_, _ = w.Write(responseBody)
		}
	}
	h.recordRequest(start, r, upstreamPath, body, responseLogCapture{
		status:        responseStatus,
		bytesWritten:  logCapture.Total(),
		contentType:   resp.Header.Get("Content-Type"),
		body:          logCapture.Bytes(),
		bodyTruncated: logCapture.Truncated(),
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

func normalizePublicErrorStatus(status int) int {
	if status == http.StatusUnavailableForLegalReasons || status == publicReviewStatus {
		return publicReviewStatus
	}
	if status == http.StatusTooManyRequests {
		return http.StatusTooManyRequests
	}
	if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func publicErrorMessage(status int) string {
	switch normalizePublicErrorStatus(status) {
	case publicReviewStatus:
		return publicReviewErrorMessage
	case http.StatusTooManyRequests:
		return publicRateLimitErrorMessage
	case http.StatusBadRequest:
		return publicClientErrorMessage
	default:
		return publicServerErrorMessage
	}
}

func publicErrorBody(upstreamPath string, status int) []byte {
	publicStatus := normalizePublicErrorStatus(status)
	message := publicErrorMessage(publicStatus)
	if upstreamPath == "/messages" {
		errorType := "invalid_request_error"
		if publicStatus == http.StatusTooManyRequests {
			errorType = "rate_limit_error"
		} else if publicStatus == http.StatusInternalServerError {
			errorType = "api_error"
		}
		body, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    errorType,
				"message": message,
			},
		})
		return body
	}

	errorType := "invalid_request_error"
	if publicStatus == http.StatusTooManyRequests {
		errorType = "rate_limit_error"
	} else if publicStatus == http.StatusInternalServerError {
		errorType = "server_error"
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errorType,
			"code":    publicStatus,
		},
	})
	return body
}

func writePublicErrorResponse(w http.ResponseWriter, status int, upstreamPath string) {
	publicStatus := normalizePublicErrorStatus(status)
	body := publicErrorBody(upstreamPath, publicStatus)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("Content-Length")
	w.Header().Del("Content-Encoding")
	w.Header().Del("ETag")
	w.Header().Del("Content-MD5")
	w.WriteHeader(publicStatus)
	_, _ = w.Write(body)
}

func writeUpstreamErrorResponse(w http.ResponseWriter, status int, upstreamPath string, upstreamBody []byte, trusted bool) {
	if !trusted {
		writePublicErrorResponse(w, status, upstreamPath)
		return
	}
	publicStatus := normalizePublicErrorStatus(status)
	body := trustedUpstreamErrorBody(upstreamPath, upstreamBody)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("Content-Length")
	w.Header().Del("Content-Encoding")
	w.Header().Del("ETag")
	w.Header().Del("Content-MD5")
	w.WriteHeader(publicStatus)
	_, _ = w.Write(body)
}

func publicUpstreamErrorBody(upstreamPath string) []byte {
	return publicErrorBody(upstreamPath, http.StatusInternalServerError)
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

type eventStreamAudit struct {
	HadError         bool
	Events           int
	ParseErrors      int
	ErrorEvents      int
	VisibleDeltas    int
	ToolDeltas       int
	CompletionTokens int64
	Done             bool
}

func (a eventStreamAudit) SuspectedSoftError() bool {
	return a.HadError || a.ErrorEvents > 0 || (a.VisibleDeltas == 0 && a.ToolDeltas == 0 && a.CompletionTokens == 0)
}

func copyEventStreamWithNormalizeAndCapture(w http.ResponseWriter, r io.Reader, capture *captureBuffer, publicModel string, upstreamPath string) eventStreamAudit {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(r)
	audit := eventStreamAudit{}
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			audit.observe(line)
			chunk, chunkHadError := normalizeEventStreamLine(line, publicModel, upstreamPath)
			if capture != nil {
				if chunkHadError {
					_, _ = capture.Write(line)
				} else {
					_, _ = capture.Write(chunk)
				}
			}
			audit.HadError = audit.HadError || chunkHadError
			_, _ = w.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return audit
		}
	}
}

func (a *eventStreamAudit) observe(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(data) == 0 {
		return
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		a.Done = true
		return
	}
	a.Events++
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		a.ParseErrors++
		return
	}
	if successPayloadHasError(payload) {
		a.ErrorEvents++
	}
	if usage, ok := payload["usage"].(map[string]any); ok {
		for _, key := range []string{"completion_tokens", "output_tokens"} {
			if value, ok := streamAuditInt64(usage[key]); ok {
				a.CompletionTokens = value
				break
			}
		}
	}
	choices, _ := payload["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if streamAuditHasVisibleText(delta["content"]) {
			a.VisibleDeltas++
		}
		if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
			a.ToolDeltas++
		}
	}
}

func streamAuditHasVisibleText(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		for _, raw := range typed {
			if streamAuditHasVisibleText(raw) {
				return true
			}
		}
	case map[string]any:
		for _, key := range []string{"text", "content"} {
			if streamAuditHasVisibleText(typed[key]) {
				return true
			}
		}
	}
	return false
}

func streamAuditInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	default:
		return 0, false
	}
}

func normalizeEventStreamLine(line []byte, publicModel string, upstreamPath string) ([]byte, bool) {
	trimmed := bytes.TrimRight(line, "\r\n")
	lineEnding := line[len(trimmed):]
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return line, false
	}
	data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return line, false
	}
	normalized, validSuccess := normalizeSuccessResponseBodyChecked(data, publicModel, upstreamPath, true)
	output := make([]byte, 0, len("data: ")+len(normalized)+len(lineEnding))
	output = append(output, "data: "...)
	output = append(output, normalized...)
	output = append(output, lineEnding...)
	return output, !validSuccess
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

func (h *Handler) forwardWithModelRetry(original *http.Request, upstreamPath string, body []byte, publicModel string) (*http.Response, error) {
	if isStructuredJSONRetryRequest(original) && (publicModel == prefixPublicModel(lingTinyFreeModel) || publicModel == kiloAutoJSONPublicModel) {
		return h.forwardWithStructuredJSONRetry(original, upstreamPath, body, publicModel)
	}
	if publicModel == prefixPublicModel(lingTinyFreeModel) {
		return h.forwardWithLingTiny429Retry(original, upstreamPath, body, publicModel)
	}
	if h.cfg.KiloAutoReliabilityEnabled && publicModel == kiloAutoJSONPublicModel && upstreamPath == lingJSONRequestPath {
		return h.forwardWithKiloAutoRetry(original, upstreamPath, body, publicModel)
	}
	return h.forward(original, upstreamPath, bytes.NewReader(body))
}

func isStructuredJSONRetryRequest(r *http.Request) bool {
	return lingJSONStructuredRetryStateFromRequest(r) != nil
}

func (h *Handler) forwardWithStructuredJSONRetry(original *http.Request, upstreamPath string, body []byte, publicModel string) (*http.Response, error) {
	state := lingJSONStructuredRetryStateFromRequest(original)
	if state == nil {
		return h.forward(original, upstreamPath, bytes.NewReader(body))
	}
	for {
		resp, err := h.forward(original, upstreamPath, bytes.NewReader(body))
		retryable := err != nil
		statusLabel := "transport"
		if resp != nil {
			statusLabel = strconv.Itoa(resp.StatusCode)
			retryable = isStructuredJSONRetryableStatus(resp.StatusCode)
		}
		if !retryable || state.retries >= len(structuredJSONRetryBackoff) {
			return resp, err
		}
		delay, ok := structuredJSONRetryDelay(state.retries, retryAfterFromResponse(resp), state.totalWait, h.retryJitter)
		if !ok {
			return resp, err
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		fmt.Printf("kilo_upstream_retry scope=structured_json path=%s model=%s retry=%d max_retries=%d status=%s wait_ms=%d retry_after_present=%t\n",
			upstreamPath,
			publicModel,
			state.retries+1,
			len(structuredJSONRetryBackoff),
			statusLabel,
			delay.Milliseconds(),
			strings.TrimSpace(retryAfterFromResponse(resp)) != "",
		)
		if waitErr := h.waitBeforeRetry(original.Context(), delay); waitErr != nil {
			return nil, waitErr
		}
		state.retries++
		state.totalWait += delay
	}
}

func retryAfterFromResponse(resp *http.Response) string {
	if resp == nil {
		return ""
	}
	return resp.Header.Get("Retry-After")
}

func isStructuredJSONRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func structuredJSONRetryDelay(retryIndex int, retryAfter string, totalWait time.Duration, jitter func(time.Duration) time.Duration) (time.Duration, bool) {
	if retryIndex < 0 || retryIndex >= len(structuredJSONRetryBackoff) || totalWait >= structuredJSONRetryBudget {
		return 0, false
	}
	delay := structuredJSONRetryBackoff[retryIndex]
	if jitter != nil {
		if extra := jitter(delay); extra > 0 {
			delay += extra
		}
	}
	if requested, ok := parseRetryAfterWithBudget(retryAfter, time.Now(), structuredJSONRetryBudget); ok && requested > delay {
		delay = requested
	}
	remaining := structuredJSONRetryBudget - totalWait
	if delay > remaining {
		delay = remaining
	}
	return delay, true
}

func (h *Handler) forwardWithLingTiny429Retry(original *http.Request, upstreamPath string, body []byte, publicModel string) (*http.Response, error) {

	totalWait := time.Duration(0)
	for attempt := 0; ; attempt++ {
		resp, err := h.forward(original, upstreamPath, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusTooManyRequests || attempt >= len(lingTiny429RetryBackoff) {
			return resp, nil
		}

		delay, ok := lingTiny429RetryDelay(
			attempt,
			resp.Header.Get("Retry-After"),
			time.Now(),
			totalWait,
			h.retryJitter,
		)
		if !ok {
			return resp, nil
		}
		if resp.Body != nil {
			_ = resp.Body.Close()
		}

		fmt.Printf("kilo_upstream_retry path=%s model=%s retry=%d max_retries=%d status=%d wait_ms=%d retry_after_present=%t\n",
			upstreamPath,
			publicModel,
			attempt+1,
			len(lingTiny429RetryBackoff),
			http.StatusTooManyRequests,
			delay.Milliseconds(),
			strings.TrimSpace(resp.Header.Get("Retry-After")) != "",
		)

		wait := h.retryWait
		if wait == nil {
			wait = waitForRetry
		}
		if err := wait(original.Context(), delay); err != nil {
			return nil, err
		}
		totalWait += delay
	}
}

func (h *Handler) forwardWithKiloAutoRetry(original *http.Request, upstreamPath string, body []byte, publicModel string) (*http.Response, error) {
	state := kiloAutoRetryStateFromContext(original.Context())
	localRetries := 0
	localWait := time.Duration(0)
	for {
		resp, err := h.forward(original, upstreamPath, bytes.NewReader(body))
		if err != nil {
			retryIndex, totalWait := kiloAutoRetryProgress(state, localRetries, localWait)
			if retryIndex >= len(kiloAutoRetryBackoff) {
				return nil, err
			}
			delay, ok := kiloAutoRetryDelay(retryIndex, "", time.Now(), totalWait, h.retryJitter)
			if !ok {
				return nil, err
			}
			fmt.Printf("kilo_upstream_retry scope=auto path=%s model=%s retry=%d max_retries=%d status=transport wait_ms=%d retry_after_present=false\n",
				upstreamPath,
				publicModel,
				retryIndex+1,
				len(kiloAutoRetryBackoff),
				delay.Milliseconds(),
			)
			if waitErr := h.waitBeforeRetry(original.Context(), delay); waitErr != nil {
				return nil, waitErr
			}
			recordKiloAutoRetryProgress(state, &localRetries, &localWait, delay)
			continue
		}

		retryIndex, totalWait := kiloAutoRetryProgress(state, localRetries, localWait)
		if !isKiloAutoRetryableStatus(resp.StatusCode) || retryIndex >= len(kiloAutoRetryBackoff) {
			return resp, nil
		}
		delay, ok := kiloAutoRetryDelay(
			retryIndex,
			resp.Header.Get("Retry-After"),
			time.Now(),
			totalWait,
			h.retryJitter,
		)
		if !ok {
			return resp, nil
		}
		if resp.Body != nil {
			_ = resp.Body.Close()
		}

		fmt.Printf("kilo_upstream_retry scope=auto path=%s model=%s retry=%d max_retries=%d status=%d wait_ms=%d retry_after_present=%t\n",
			upstreamPath,
			publicModel,
			retryIndex+1,
			len(kiloAutoRetryBackoff),
			resp.StatusCode,
			delay.Milliseconds(),
			strings.TrimSpace(resp.Header.Get("Retry-After")) != "",
		)

		if err := h.waitBeforeRetry(original.Context(), delay); err != nil {
			return nil, err
		}
		recordKiloAutoRetryProgress(state, &localRetries, &localWait, delay)
	}
}

func kiloAutoRetryProgress(state *kiloAutoRetryState, localRetries int, localWait time.Duration) (int, time.Duration) {
	if state != nil {
		return state.rawRetries, state.totalWait
	}
	return localRetries, localWait
}

func recordKiloAutoRetryProgress(state *kiloAutoRetryState, localRetries *int, localWait *time.Duration, delay time.Duration) {
	if state != nil {
		state.rawRetries++
		state.totalWait += delay
		return
	}
	(*localRetries)++
	*localWait += delay
}

func (h *Handler) waitBeforeRetry(ctx context.Context, delay time.Duration) error {
	wait := h.retryWait
	if wait == nil {
		wait = waitForRetry
	}
	return wait(ctx, delay)
}

func isKiloAutoRetryableStatus(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func parseRetryAfter(raw string, now time.Time) (time.Duration, bool) {
	return parseRetryAfterWithBudget(raw, now, lingTiny429RetryBudget)
}

func parseRetryAfterWithBudget(raw string, now time.Time, budget time.Duration) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds >= 0 {
		maxSeconds := int64(budget / time.Second)
		if seconds > maxSeconds {
			seconds = maxSeconds
		}
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(raw)
	if err != nil {
		return 0, false
	}
	delay := when.Sub(now)
	if delay < 0 {
		delay = 0
	}
	if delay > budget {
		delay = budget
	}
	return delay, true
}

func kiloAutoRetryDelay(retryIndex int, retryAfter string, now time.Time, totalWait time.Duration, jitter func(time.Duration) time.Duration) (time.Duration, bool) {
	if retryIndex < 0 || retryIndex >= len(kiloAutoRetryBackoff) || totalWait >= kiloAutoRetryBudget {
		return 0, false
	}
	base := kiloAutoRetryBackoff[retryIndex]
	delay := base
	if jitter != nil {
		if extra := jitter(base); extra > 0 {
			delay += extra
		}
	}
	if requested, ok := parseRetryAfterWithBudget(retryAfter, now, kiloAutoRetryBudget); ok && requested > delay {
		delay = requested
	}
	remaining := kiloAutoRetryBudget - totalWait
	if delay > remaining {
		delay = remaining
	}
	return delay, true
}

func lingTiny429RetryDelay(retryIndex int, retryAfter string, now time.Time, totalWait time.Duration, jitter func(time.Duration) time.Duration) (time.Duration, bool) {
	if retryIndex < 0 || retryIndex >= len(lingTiny429RetryBackoff) || totalWait >= lingTiny429RetryBudget {
		return 0, false
	}

	base := lingTiny429RetryBackoff[retryIndex]
	delay := base
	if jitter != nil {
		if extra := jitter(base); extra > 0 {
			delay += extra
		}
	}
	if requested, ok := parseRetryAfter(retryAfter, now); ok && requested > delay {
		delay = requested
	}

	remaining := lingTiny429RetryBudget - totalWait
	if delay > remaining {
		delay = remaining
	}
	return delay, true
}

func defaultRetryJitter(base time.Duration) time.Duration {
	maxJitter := base / 5
	if maxJitter <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(maxJitter) + 1))
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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

func writeJSONError(w http.ResponseWriter, status int, _ string, _ string) {
	writePublicErrorResponse(w, status, "")
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
