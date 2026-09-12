package adapter

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Only enumerated public catalog IDs may enter telemetry. Never log arbitrary
// model/provider strings, routing diagnostics, user content or tool arguments.
// New catalog members remain unknown until explicitly reviewed here.
func knownKiloAutoModel(value any) string {
	model, _ := value.(string)
	model = strings.TrimPrefix(model, publicModelPrefix)
	switch model {
	case "dots-studio/dots-3-note-preview:free", "inclusionai/ling-3.0-flash-vl:free",
		"nex-agi/nex-n2.5-pro:free", "nvidia/nemotron-3-ultra-550b-a55b:free", "poolside/laguna-s-2.1:free":
		return model
	default:
		return ""
	}
}

type kiloAutoTelemetryKey struct{}

type kiloAutoTelemetryAttempt struct {
	Number         int    `json:"number"`
	Stage          string `json:"stage"`
	RawRetry       bool   `json:"raw_retry"`
	Status         int    `json:"status"`
	HeadersMS      int64  `json:"headers_ms"`
	DurationMS     int64  `json:"duration_ms"`
	Model          string `json:"model"`
	ModelConflict  bool   `json:"model_conflict,omitempty"`
	UnknownModel   bool   `json:"unknown_model_seen,omitempty"`
	Visible        bool   `json:"visible"`
	Terminal       bool   `json:"terminal"`
	LengthLimited  bool   `json:"length_limited"`
	ResponseError  bool   `json:"response_error"`
	TransportError bool   `json:"transport_error"`
	ReadFailed     bool   `json:"read_failed"`
	started        time.Time
}

type kiloAutoTelemetryRepair struct {
	Used         bool   `json:"used"`
	FirstFailure string `json:"first_failure"`
	FinalFailure string `json:"final_failure"`
}

type kiloAutoTelemetryJSON struct {
	RepairUsed         bool `json:"repair_used"`
	Validated          bool `json:"validated"`
	ObjectFallback     bool `json:"object_fallback"`
	ReasoningSalvaged  bool `json:"reasoning_salvaged"`
	ValidationFailures int  `json:"validation_failures"`
}

type kiloAutoTelemetryRecord struct {
	Version     int                         `json:"version"`
	Timestamp   string                      `json:"timestamp"`
	TraceID     string                      `json:"trace_id"`
	RequestHash string                      `json:"request_hash,omitempty"`
	Protocol    string                      `json:"protocol"`
	Stream      bool                        `json:"stream"`
	HasTools    bool                        `json:"has_tools"`
	Format      string                      `json:"format"`
	OutputLimit int64                       `json:"output_limit,omitempty"`
	Status      int                         `json:"status"`
	DurationMS  int64                       `json:"duration_ms"`
	RetryWaitMS int64                       `json:"retry_wait_ms"`
	Outcome     string                      `json:"outcome"`
	Attempts    []*kiloAutoTelemetryAttempt `json:"attempts"`
	Dropped     int                         `json:"attempts_not_recorded,omitempty"`
	Reliability *kiloAutoTelemetryRepair    `json:"reliability,omitempty"`
	JSON        *kiloAutoTelemetryJSON      `json:"json,omitempty"`
}

// The context belongs to one synchronous handler chain. No background readers,
// unbounded buffers, response-body capture or shared mutable request state.
type kiloAutoTelemetry struct {
	record kiloAutoTelemetryRecord
	active bool
	stage  string
	last   *kiloAutoTelemetryAttempt
}

type kiloAutoTelemetryHandler struct {
	next http.Handler
	emit func(kiloAutoTelemetryRecord)
}

func newKiloAutoTelemetryHandler(next http.Handler) *kiloAutoTelemetryHandler {
	return &kiloAutoTelemetryHandler{next: next, emit: func(record kiloAutoTelemetryRecord) {
		if data, err := json.Marshal(record); err == nil {
			fmt.Printf("kilo_auto_telemetry %s\n", data)
		}
	}}
}

func (h *kiloAutoTelemetryHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := normalizePath(r.URL.Path)
	if r.Method != http.MethodPost || (path != "/chat/completions" && path != "/messages" && path != "/responses") {
		h.next.ServeHTTP(w, r)
		return
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		h.next.ServeHTTP(w, r)
		return
	}
	trace := &kiloAutoTelemetry{stage: "initial", record: kiloAutoTelemetryRecord{
		Version: 1, TraceID: hex.EncodeToString(id[:]), Protocol: path, Format: "none",
		Attempts: make([]*kiloAutoTelemetryAttempt, 0, 4),
	}}
	// Correlation is optional and one-way. Never persist a client supplied ID.
	if requestID := r.Header.Get("X-Request-Id"); requestID != "" && len(requestID) <= 1024 {
		digest := sha256.Sum256([]byte(requestID))
		trace.record.RequestHash = hex.EncodeToString(digest[:16])
	}
	r = r.WithContext(context.WithValue(r.Context(), kiloAutoTelemetryKey{}, trace))
	started := time.Now()
	status, size := http.StatusOK, int64(0)
	lw := &loggingResponseWriter{ResponseWriter: w, status: &status, bytesWritten: &size}
	h.next.ServeHTTP(lw, r)
	if !trace.active {
		return
	}
	trace.record.Status = status
	trace.record.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	trace.record.DurationMS = time.Since(started).Milliseconds()
	trace.record.Outcome = trace.outcome(r.Context())
	h.emit(trace.record)
}

func activeKiloAutoTelemetry(ctx context.Context) *kiloAutoTelemetry {
	trace, _ := ctx.Value(kiloAutoTelemetryKey{}).(*kiloAutoTelemetry)
	if trace == nil || !trace.active {
		return nil
	}
	return trace
}

// Reuse existing body reads. The first observation preserves client controls,
// before compatibility, trimming or repair changes them.
func observeKiloAutoTelemetryRequest(r *http.Request, body []byte) {
	trace, _ := r.Context().Value(kiloAutoTelemetryKey{}).(*kiloAutoTelemetry)
	if trace == nil || trace.active {
		return
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		return
	}
	model, _ := payload["model"].(string)
	if model != kiloAutoFreeModel && model != kiloAutoJSONPublicModel {
		return
	}
	trace.active = true
	trace.record.Stream, _ = payload["stream"].(bool)
	trace.record.HasTools = hasLingJSONTools(payload)
	if format, ok := payload["response_format"].(map[string]any); ok {
		switch format["type"] {
		case "json_object":
			trace.record.Format = "json_object"
		case "json_schema":
			trace.record.Format = "json_schema"
		default:
			trace.record.Format = "other"
		}
	}
	for _, field := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if n, ok := lingJSONInteger(payload[field]); ok && n > 0 && n <= 1<<31-1 {
			trace.record.OutputLimit = n
			break
		}
	}
}

func beginKiloAutoTelemetryAttempt(ctx context.Context) *kiloAutoTelemetryAttempt {
	trace := activeKiloAutoTelemetry(ctx)
	if trace == nil {
		return nil
	}
	attempt := &kiloAutoTelemetryAttempt{Number: len(trace.record.Attempts) + trace.record.Dropped + 1,
		Stage: trace.stage, Model: "unknown", started: time.Now()}
	attempt.RawRetry = trace.last != nil && trace.last.Stage == trace.stage
	trace.last = attempt
	if len(trace.record.Attempts) < 8 {
		trace.record.Attempts = append(trace.record.Attempts, attempt)
	} else {
		trace.record.Dropped++
	}
	return attempt
}

func (trace *kiloAutoTelemetry) current() *kiloAutoTelemetryAttempt { return trace.last }

func finishKiloAutoTelemetryAttempt(ctx context.Context) {
	if trace := activeKiloAutoTelemetry(ctx); trace != nil && trace.last != nil {
		trace.last.DurationMS = time.Since(trace.last.started).Milliseconds()
	}
}

func markKiloAutoTelemetryRepair(ctx context.Context, stage string) {
	if trace := activeKiloAutoTelemetry(ctx); trace != nil {
		trace.stage = stage
	}
}

func kiloAutoTelemetryObserver(ctx context.Context) func(map[string]any) {
	trace := activeKiloAutoTelemetry(ctx)
	if trace == nil || trace.last == nil {
		return nil
	}
	attempt := trace.last
	return func(payload map[string]any) { attempt.observe(payload) }
}

func (a *kiloAutoTelemetryAttempt) observe(payload map[string]any) {
	// Only actual protocol envelopes carry model evidence. Never recurse into
	// provider_metadata, routing.attempts, content, tool input or arbitrary JSON.
	envelope := payload
	if response, ok := payload["response"].(map[string]any); ok {
		envelope = response
	} else if payload["type"] == "message_start" {
		if message, ok := payload["message"].(map[string]any); ok {
			envelope = message
		}
	}
	rawModel, _ := envelope["model"].(string)
	model := knownKiloAutoModel(rawModel)
	if model == "" && rawModel != "" && rawModel != kiloAutoFreeModel && rawModel != kiloAutoJSONPublicModel {
		a.UnknownModel, a.Model = true, "unknown"
	}
	if model != "" && !a.ModelConflict && !a.UnknownModel {
		if a.Model != "unknown" && a.Model != model {
			a.ModelConflict, a.Model = true, "unknown"
		} else {
			a.Model = model
		}
	}
	a.ResponseError = a.ResponseError || successPayloadHasError(payload)
	choices, _ := envelope["choices"].([]any)
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		finish, _ := choice["finish_reason"].(string)
		a.observeFinish(finish)
		for _, key := range []string{"message", "delta"} {
			message, _ := choice[key].(map[string]any)
			a.Visible = a.Visible || streamAuditHasVisibleText(message["content"])
			if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
				a.Visible = true
			}
		}
	}
	if stop, ok := envelope["stop_reason"].(string); ok {
		a.observeFinish(stop)
	}
	if content, ok := envelope["content"].([]any); ok {
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			a.Visible = a.Visible || streamAuditHasVisibleText(block["text"]) || block["type"] == "tool_use"
		}
	}
	if output, ok := envelope["output"].([]any); ok {
		for _, raw := range output {
			item, _ := raw.(map[string]any)
			a.Visible = a.Visible || streamAuditHasVisibleText(item["content"]) || item["type"] == "function_call"
		}
	}
	if envelope["status"] == "completed" {
		a.Terminal = true
	}
	switch payload["type"] {
	case "response.completed", "message_stop":
		a.Terminal = true
	case "response.output_text.delta":
		a.Visible = a.Visible || streamAuditHasVisibleText(payload["delta"])
	case "response.function_call_arguments.delta":
		a.Visible = true
	case "content_block_start":
		block, _ := payload["content_block"].(map[string]any)
		a.Visible = a.Visible || block["type"] == "tool_use"
	case "content_block_delta":
		delta, _ := payload["delta"].(map[string]any)
		a.Visible = a.Visible || streamAuditHasVisibleText(delta["text"]) || delta["type"] == "input_json_delta"
	case "message_delta":
		delta, _ := payload["delta"].(map[string]any)
		stop, _ := delta["stop_reason"].(string)
		a.observeFinish(stop)
	}
}

func (a *kiloAutoTelemetryAttempt) observeFinish(finish string) {
	switch finish {
	case "stop", "end_turn", "tool_calls", "tool_use", "function_call", "stop_sequence":
		a.Terminal = true
	case "length", "max_tokens", "max_output_tokens":
		a.LengthLimited = true
	}
}

func (trace *kiloAutoTelemetry) outcome(ctx context.Context) string {
	if ctx.Err() != nil {
		return "cancelled"
	}
	if trace.record.Status < 200 || trace.record.Status >= 300 {
		return "http_error"
	}
	if stats := trace.record.JSON; stats != nil && stats.Validated {
		if stats.ObjectFallback {
			return "local_object_fallback"
		}
		return "json_validated"
	}
	if stats := trace.record.Reliability; stats != nil && stats.FinalFailure == "ok" {
		return "visible_complete"
	}
	if a := trace.last; a != nil {
		if a.ResponseError || a.ReadFailed || a.TransportError {
			return "response_error"
		}
		if a.Visible && a.Terminal && !a.LengthLimited {
			// Protocol evidence, not a claim about tool execution or task quality.
			return "terminal_observed"
		}
	}
	return "unverified"
}
