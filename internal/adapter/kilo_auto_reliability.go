package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	kiloAutoFirstReasoningTokens  = 512
	kiloAutoRepairReasoningTokens = 256
	kiloAutoRepairOutputTokens    = 10000
	kiloAutoStreamPreludeBytes    = 1 << 20
	kiloAutoRepairInstruction     = "Reliability retry: provide a concise, complete visible answer or a valid tool call. Do not spend the response budget on hidden analysis. If code is requested, return the complete code."
	kiloAutoRepairUserInstruction = "Retry the immediately preceding request now. Return a concise, complete visible final answer, or a valid tool call if tools are needed. Do not repeat hidden analysis."
)

type kiloAutoReliabilityHandler struct {
	next                         http.Handler
	maxBodyBytes                 int64
	kiloAutoRequestCompatEnabled bool
}

type kiloAutoRetryStateKey struct{}

type kiloAutoRetryState struct {
	rawRetries int
	totalWait  time.Duration
}

type kiloAutoReliabilityStats struct {
	Stream                bool
	Attempts              int
	RepairUsed            bool
	FirstReasoningCap     bool
	FirstFailureClass     string
	FinalFailureClass     string
	FinalStatus           int
	StreamCommitted       bool
	StreamPreludeOverflow bool
	Started               time.Time
}

type kiloAutoResponseAssessment struct {
	Retry       bool
	Policy      bool
	Failure     string
	Finish      string
	VisibleText int
	ToolCalls   int
}

func newKiloAutoReliabilityHandler(next http.Handler, maxBodyBytes int64) http.Handler {
	return newKiloAutoReliabilityHandlerWithRequestCompat(next, maxBodyBytes, false)
}

func newKiloAutoReliabilityHandlerWithRequestCompat(next http.Handler, maxBodyBytes int64, enabled bool) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	if maxBodyBytes <= 0 {
		maxBodyBytes = 10 << 20
	}
	return &kiloAutoReliabilityHandler{
		next:                         next,
		maxBodyBytes:                 maxBodyBytes,
		kiloAutoRequestCompatEnabled: enabled,
	}
}

func (h *kiloAutoReliabilityHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || normalizePath(r.URL.Path) != lingJSONRequestPath {
		h.next.ServeHTTP(w, r)
		return
	}

	body, err := readAndRestoreLingJSONBody(r, h.maxBodyBytes)
	if err != nil {
		writePublicErrorResponse(w, http.StatusBadRequest, lingJSONRequestPath)
		return
	}
	observeKiloAutoTelemetryRequest(r, body)
	payload, stream, tools, autoRequest, matched := parseKiloAutoReliabilityRequest(body)
	if !autoRequest {
		h.next.ServeHTTP(w, r)
		return
	}
	if h.kiloAutoRequestCompatEnabled {
		stats := normalizeKiloAutoChatPayload(payload)
		logKiloAutoRequestCompat(stats)
		if stats.Changed {
			body, err = json.Marshal(payload)
			if err != nil {
				writePublicErrorResponse(w, http.StatusBadRequest, lingJSONRequestPath)
				return
			}
			restoreLingJSONBody(r, body)
			stream, _ = payload["stream"].(bool)
			tools = hasLingJSONTools(payload)
			_, hasResponseFormat := payload["response_format"]
			matched = !hasResponseFormat
		}
	}
	r = withKiloAutoRetryState(r)
	if !matched {
		h.next.ServeHTTP(w, r)
		return
	}

	stats := &kiloAutoReliabilityStats{Stream: stream, Started: time.Now()}
	defer logKiloAutoReliability(stats)
	defer func() {
		if trace := activeKiloAutoTelemetry(r.Context()); trace != nil {
			trace.record.Reliability = &kiloAutoTelemetryRepair{Used: stats.RepairUsed, FirstFailure: stats.FirstFailureClass, FinalFailure: stats.FinalFailureClass}
		}
	}()

	firstBody := body
	if !tools {
		firstPayload, cloneErr := cloneLingJSONPayload(payload)
		if cloneErr != nil {
			stats.FinalFailureClass = "request_clone_failed"
			stats.FinalStatus = http.StatusBadRequest
			writePublicErrorResponse(w, http.StatusBadRequest, lingJSONRequestPath)
			return
		}
		if setKiloAutoReasoningLimit(firstPayload, kiloAutoFirstReasoningTokens, false) {
			firstBody, err = json.Marshal(firstPayload)
			if err != nil {
				stats.FinalFailureClass = "request_encode_failed"
				stats.FinalStatus = http.StatusBadRequest
				writePublicErrorResponse(w, http.StatusBadRequest, lingJSONRequestPath)
				return
			}
			stats.FirstReasoningCap = true
		}
	}

	if stream {
		h.serveKiloAutoStream(w, r, payload, firstBody, stats)
		return
	}
	h.serveKiloAutoNonStream(w, r, payload, firstBody, stats)
}

func parseKiloAutoReliabilityRequest(body []byte) (map[string]any, bool, bool, bool, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, false, false, false, false
	}
	model, ok := payload["model"].(string)
	if !ok {
		return payload, false, false, false, false
	}
	model = strings.TrimSpace(model)
	if model != kiloAutoFreeModel && model != kiloAutoJSONPublicModel {
		return payload, false, false, false, false
	}
	if _, exists := payload["response_format"]; exists {
		return payload, false, false, true, false
	}
	stream, _ := payload["stream"].(bool)
	tools := hasLingJSONTools(payload)
	return payload, stream, tools, true, true
}

func withKiloAutoRetryState(r *http.Request) *http.Request {
	if r == nil {
		return r
	}
	if kiloAutoRetryStateFromContext(r.Context()) != nil {
		return r
	}
	state := &kiloAutoRetryState{}
	return r.WithContext(context.WithValue(r.Context(), kiloAutoRetryStateKey{}, state))
}

func kiloAutoRetryStateFromContext(ctx context.Context) *kiloAutoRetryState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(kiloAutoRetryStateKey{}).(*kiloAutoRetryState)
	return state
}

func setKiloAutoReasoningLimit(payload map[string]any, limit int, force bool) bool {
	if payload == nil || limit <= 0 {
		return false
	}
	raw, exists := payload["reasoning"]
	if !exists {
		payload["reasoning"] = map[string]any{"max_tokens": json.Number(strconv.Itoa(limit))}
		return true
	}
	if !force {
		switch typed := raw.(type) {
		case bool:
			if !typed {
				return false
			}
		case string:
			switch strings.ToLower(strings.TrimSpace(typed)) {
			case "none", "off", "disabled", "false":
				return false
			}
		case map[string]any:
			if enabled, ok := typed["enabled"].(bool); ok && !enabled {
				return false
			}
		}
	}

	reasoning, ok := raw.(map[string]any)
	if !ok {
		reasoning = map[string]any{}
		payload["reasoning"] = reasoning
	}
	current, valid := lingJSONInteger(reasoning["max_tokens"])
	if !force && valid && current > 0 && current <= int64(limit) {
		return false
	}
	reasoning["max_tokens"] = json.Number(strconv.Itoa(limit))
	return true
}

func buildKiloAutoRepairBody(original map[string]any) ([]byte, error) {
	payload, err := cloneLingJSONPayload(original)
	if err != nil {
		return nil, err
	}
	setKiloAutoReasoningLimit(payload, kiloAutoRepairReasoningTokens, true)
	raiseKiloAutoRepairOutputFloor(payload, kiloAutoRepairOutputTokens)
	payload["temperature"] = json.Number("0")
	payload["n"] = json.Number("1")

	messages, ok := payload["messages"].([]any)
	if !ok || len(messages) == 0 {
		return nil, errors.New("messages missing")
	}
	insertAt := 0
	for insertAt < len(messages) {
		message, ok := messages[insertAt].(map[string]any)
		if !ok {
			break
		}
		role := strings.ToLower(strings.TrimSpace(fmt.Sprint(message["role"])))
		if role != "system" {
			break
		}
		insertAt++
	}
	repaired := make([]any, 0, len(messages)+2)
	repaired = append(repaired, messages[:insertAt]...)
	repaired = append(repaired, map[string]any{"role": "system", "content": kiloAutoRepairInstruction})
	repaired = append(repaired, messages[insertAt:]...)
	repaired = append(repaired, map[string]any{"role": "user", "content": kiloAutoRepairUserInstruction})
	payload["messages"] = repaired
	return json.Marshal(payload)
}

func raiseKiloAutoRepairOutputFloor(payload map[string]any, minimum int) {
	found := false
	for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		value, exists := payload[field]
		if !exists {
			continue
		}
		current, ok := lingJSONInteger(value)
		if !ok || current <= 0 {
			continue
		}
		found = true
		if current < int64(minimum) {
			payload[field] = json.Number(strconv.Itoa(minimum))
		}
	}
	if !found {
		payload["max_tokens"] = json.Number(strconv.Itoa(minimum))
	}
}

func (h *kiloAutoReliabilityHandler) serveKiloAutoNonStream(w http.ResponseWriter, r *http.Request, original map[string]any, firstBody []byte, stats *kiloAutoReliabilityStats) {
	first := newLingJSONBufferedWriter()
	h.next.ServeHTTP(first, cloneLingJSONRequest(r, firstBody))
	stats.Attempts = 1
	stats.FinalStatus = first.Status()
	if first.Status() < http.StatusOK || first.Status() >= http.StatusMultipleChoices {
		stats.FinalFailureClass = "upstream_error"
		copyLingJSONBufferedResponse(w, first, first.body.Bytes())
		return
	}

	assessment := assessKiloAutoNonStream(first.body.Bytes())
	stats.FirstFailureClass = assessment.Failure
	if !assessment.Retry {
		stats.FinalFailureClass = assessment.Failure
		copyLingJSONBufferedResponse(w, first, first.body.Bytes())
		return
	}

	repairBody, err := buildKiloAutoRepairBody(original)
	if err != nil {
		stats.FinalFailureClass = "repair_build_failed"
		stats.FinalStatus = http.StatusInternalServerError
		writePublicErrorResponse(w, http.StatusInternalServerError, lingJSONRequestPath)
		return
	}
	stats.RepairUsed = true
	markKiloAutoTelemetryRepair(r.Context(), "visible_repair")
	second := newLingJSONBufferedWriter()
	h.next.ServeHTTP(second, cloneLingJSONRequest(r, repairBody))
	stats.Attempts = 2
	stats.FinalStatus = second.Status()
	if second.Status() < http.StatusOK || second.Status() >= http.StatusMultipleChoices {
		stats.FinalFailureClass = "upstream_error"
		copyLingJSONBufferedResponse(w, second, second.body.Bytes())
		return
	}

	finalAssessment := assessKiloAutoNonStream(second.body.Bytes())
	stats.FinalFailureClass = finalAssessment.Failure
	if finalAssessment.Retry {
		stats.FinalStatus = http.StatusInternalServerError
		writePublicErrorResponse(w, http.StatusInternalServerError, lingJSONRequestPath)
		return
	}
	copyLingJSONBufferedResponse(w, second, second.body.Bytes())
}

func assessKiloAutoNonStream(body []byte) kiloAutoResponseAssessment {
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		return kiloAutoResponseAssessment{Retry: true, Failure: "invalid_envelope"}
	}
	choices, _ := envelope["choices"].([]any)
	if len(choices) == 0 {
		return kiloAutoResponseAssessment{Retry: true, Failure: "missing_choice"}
	}
	choice, _ := choices[0].(map[string]any)
	finish := strings.ToLower(strings.TrimSpace(fmt.Sprint(choice["finish_reason"])))
	if finish == "<nil>" {
		finish = ""
	}
	if isKiloAutoPolicyFinish(finish) {
		return kiloAutoResponseAssessment{Policy: true, Failure: "policy_finish", Finish: finish}
	}
	message, _ := choice["message"].(map[string]any)
	content := strings.TrimSpace(kiloAutoContentText(message["content"]))
	toolCalls := countValidKiloAutoToolCalls(message["tool_calls"])
	truncated := isKiloAutoLengthFinish(finish)
	missingFinish := finish == ""
	if content == "" && toolCalls == 0 {
		return kiloAutoResponseAssessment{Retry: true, Failure: "empty_visible_answer", Finish: finish}
	}
	if truncated {
		return kiloAutoResponseAssessment{Retry: true, Failure: "truncated", Finish: finish, VisibleText: len(content), ToolCalls: toolCalls}
	}
	if missingFinish {
		return kiloAutoResponseAssessment{Retry: true, Failure: "missing_finish", VisibleText: len(content), ToolCalls: toolCalls}
	}
	return kiloAutoResponseAssessment{Failure: "ok", Finish: finish, VisibleText: len(content), ToolCalls: toolCalls}
}

func kiloAutoContentText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, raw := range typed {
			block, _ := raw.(map[string]any)
			if text, ok := block["text"].(string); ok {
				builder.WriteString(text)
			}
		}
		return builder.String()
	default:
		return ""
	}
}

func countValidKiloAutoToolCalls(value any) int {
	calls, _ := value.([]any)
	valid := 0
	for _, raw := range calls {
		call, _ := raw.(map[string]any)
		function, _ := call["function"].(map[string]any)
		name, ok := function["name"].(string)
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		arguments, exists := function["arguments"]
		if !exists {
			continue
		}
		switch typed := arguments.(type) {
		case map[string]any:
			valid++
		case string:
			var parsed map[string]any
			if json.Unmarshal([]byte(typed), &parsed) == nil {
				valid++
			}
		}
	}
	return valid
}

func isKiloAutoLengthFinish(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "length", "max_tokens", "max_output_tokens":
		return true
	default:
		return false
	}
}

func isKiloAutoPolicyFinish(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(value, "filter") || strings.Contains(value, "safety") || strings.Contains(value, "guardrail") || strings.Contains(value, "blocked")
}

type kiloAutoStreamPreludeWriter struct {
	dst          http.ResponseWriter
	header       http.Header
	status       int
	pending      bytes.Buffer
	scan         bytes.Buffer
	limit        int
	committed    bool
	meaningful   bool
	overflow     bool
	errorEvent   bool
	policyFinish bool
	finish       string
}

func newKiloAutoStreamPreludeWriter(dst http.ResponseWriter, limit int) *kiloAutoStreamPreludeWriter {
	if limit <= 0 {
		limit = kiloAutoStreamPreludeBytes
	}
	return &kiloAutoStreamPreludeWriter{dst: dst, header: make(http.Header), limit: limit}
}

func (w *kiloAutoStreamPreludeWriter) Header() http.Header {
	return w.header
}

func (w *kiloAutoStreamPreludeWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *kiloAutoStreamPreludeWriter) Write(data []byte) (int, error) {
	if w.committed {
		return w.dst.Write(data)
	}
	if !w.overflow {
		_, _ = w.pending.Write(data)
	}
	_, _ = w.scan.Write(data)
	w.scanLines()
	if w.overflow {
		return len(data), nil
	}
	if w.meaningful {
		if err := w.Commit(); err != nil {
			return len(data), err
		}
	} else if w.pending.Len() >= w.limit {
		w.overflow = true
		w.pending.Reset()
	}
	return len(data), nil
}

func (w *kiloAutoStreamPreludeWriter) Flush() {
	if !w.committed {
		return
	}
	if flusher, ok := w.dst.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *kiloAutoStreamPreludeWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *kiloAutoStreamPreludeWriter) Commit() error {
	if w.committed {
		return nil
	}
	copyLingJSONHeaders(w.dst.Header(), w.header)
	w.dst.Header().Del("Content-Length")
	w.dst.WriteHeader(w.Status())
	w.committed = true
	if w.pending.Len() > 0 {
		if _, err := io.Copy(w.dst, bytes.NewReader(w.pending.Bytes())); err != nil {
			return err
		}
		w.pending.Reset()
	}
	if flusher, ok := w.dst.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func (w *kiloAutoStreamPreludeWriter) scanLines() {
	for {
		line, err := w.scan.ReadString('\n')
		if err != nil {
			if len(line) > 0 {
				_, _ = w.scan.WriteString(line)
			}
			return
		}
		w.inspectSSELine([]byte(line))
	}
}

func (w *kiloAutoStreamPreludeWriter) inspectSSELine(line []byte) {
	trimmed := bytes.TrimSpace(line)
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return
	}
	data := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
		return
	}
	var chunk map[string]any
	if json.Unmarshal(data, &chunk) != nil {
		return
	}
	if meaningfulErrorValue(chunk["error"]) {
		w.errorEvent = true
		return
	}
	choices, _ := chunk["choices"].([]any)
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		finish := strings.ToLower(strings.TrimSpace(fmt.Sprint(choice["finish_reason"])))
		if finish != "" && finish != "<nil>" {
			w.finish = finish
			w.policyFinish = w.policyFinish || isKiloAutoPolicyFinish(finish)
		}
		delta, _ := choice["delta"].(map[string]any)
		if strings.TrimSpace(kiloAutoContentText(delta["content"])) != "" {
			w.meaningful = true
		}
		if calls, ok := delta["tool_calls"].([]any); ok && len(calls) > 0 {
			w.meaningful = true
		}
		if functionCall, ok := delta["function_call"].(map[string]any); ok && len(functionCall) > 0 {
			w.meaningful = true
		}
	}
}

func (h *kiloAutoReliabilityHandler) serveKiloAutoStream(w http.ResponseWriter, r *http.Request, original map[string]any, firstBody []byte, stats *kiloAutoReliabilityStats) {
	first := newKiloAutoStreamPreludeWriter(w, kiloAutoStreamPreludeBytes)
	h.next.ServeHTTP(first, cloneLingJSONRequest(r, firstBody))
	stats.Attempts = 1
	stats.FinalStatus = first.Status()
	stats.StreamCommitted = first.committed
	stats.StreamPreludeOverflow = first.overflow
	if first.committed {
		stats.FinalFailureClass = "committed"
		return
	}
	if first.overflow && (first.errorEvent || first.policyFinish) {
		status := http.StatusInternalServerError
		if first.policyFinish {
			status = publicReviewStatus
		}
		stats.FinalFailureClass = "overflow_not_retryable"
		stats.FinalStatus = status
		writePublicErrorResponse(w, status, lingJSONRequestPath)
		return
	}
	if first.Status() < http.StatusOK || first.Status() >= http.StatusMultipleChoices || !isEventStreamResponse(first.header) || first.errorEvent || first.policyFinish {
		stats.FinalFailureClass = "not_retryable"
		_ = first.Commit()
		return
	}

	stats.FirstFailureClass = "empty_visible_stream"
	if first.overflow {
		stats.FirstFailureClass = "prelude_overflow"
	}
	repairBody, err := buildKiloAutoRepairBody(original)
	if err != nil {
		stats.FinalFailureClass = "repair_build_failed"
		stats.FinalStatus = http.StatusInternalServerError
		writePublicErrorResponse(w, http.StatusInternalServerError, lingJSONRequestPath)
		return
	}
	stats.RepairUsed = true
	markKiloAutoTelemetryRepair(r.Context(), "visible_repair")
	second := newKiloAutoStreamPreludeWriter(w, kiloAutoStreamPreludeBytes)
	h.next.ServeHTTP(second, cloneLingJSONRequest(r, repairBody))
	stats.Attempts = 2
	stats.FinalStatus = second.Status()
	stats.StreamCommitted = second.committed
	stats.StreamPreludeOverflow = stats.StreamPreludeOverflow || second.overflow
	if second.committed {
		stats.FinalFailureClass = "ok"
		return
	}
	if second.overflow && (second.errorEvent || second.policyFinish) {
		status := http.StatusInternalServerError
		if second.policyFinish {
			status = publicReviewStatus
		}
		stats.FinalFailureClass = "overflow_not_retryable"
		stats.FinalStatus = status
		writePublicErrorResponse(w, status, lingJSONRequestPath)
		return
	}
	if second.Status() < http.StatusOK || second.Status() >= http.StatusMultipleChoices || !isEventStreamResponse(second.header) || second.errorEvent || second.policyFinish {
		stats.FinalFailureClass = "not_retryable"
		_ = second.Commit()
		return
	}

	stats.FinalFailureClass = "empty_visible_stream"
	if second.overflow {
		stats.FinalFailureClass = "prelude_overflow"
	}
	stats.FinalStatus = http.StatusInternalServerError
	writePublicErrorResponse(w, http.StatusInternalServerError, lingJSONRequestPath)
}

func logKiloAutoReliability(stats *kiloAutoReliabilityStats) {
	if stats == nil {
		return
	}
	fmt.Printf("kilo_auto_reliability stream=%t attempts=%d repair_used=%t first_reasoning_cap=%t first_failure=%s final_failure=%s final_status=%d stream_committed=%t prelude_overflow=%t duration_ms=%d\n",
		stats.Stream,
		stats.Attempts,
		stats.RepairUsed,
		stats.FirstReasoningCap,
		stats.FirstFailureClass,
		stats.FinalFailureClass,
		stats.FinalStatus,
		stats.StreamCommitted,
		stats.StreamPreludeOverflow,
		time.Since(stats.Started).Milliseconds(),
	)
}
