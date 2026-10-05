package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Optional local control file; absent, invalid, oversized or disabled means
// exact legacy trimming. Updating this file never restarts a live service.
type contextRecallPolicy struct {
	StepToolsMode    string `json:"step_tools_mode,omitempty"`
	StepToolsPercent int    `json:"step_tools_percent,omitempty"`
	Mode             string `json:"mode"`
	Percent          int    `json:"percent"`
	FragmentPercent  int    `json:"fragment_percent"`
	RepeatedPercent  int    `json:"repeated_percent"`
}

type contextRecallControl struct {
	stepTools  *stepToolCounterPool
	path       string
	engine     *contextRecallEngine
	policy     atomic.Pointer[contextRecallPolicy]
	nextReadNS atomic.Int64
	readMu     sync.Mutex
	emit       func(contextRecallEvent)
}

type contextRecallEvent struct {
	ResponseBodySHA256      string `json:"response_body_sha256,omitempty"`
	ResponseBodyBytes       int64  `json:"response_body_bytes,omitempty"`
	ResponseHashLimited     bool   `json:"response_hash_limited,omitempty"`
	ResponseWriteFailed     bool   `json:"response_write_failed,omitempty"`
	ResponseRequestCanceled bool   `json:"response_request_canceled,omitempty"`
	ToolsEligible           bool   `json:"tools_eligible,omitempty"`
	ToolsMode               string `json:"tools_mode,omitempty"`
	ToolsPercent            int    `json:"tools_percent,omitempty"`
	ToolsModel              string `json:"tools_model,omitempty"`
	ToolsAttempted          bool   `json:"tools_attempted,omitempty"`
	ToolsApplied            bool   `json:"tools_budget_applied,omitempty"`
	ToolsReason             string `json:"tools_reason,omitempty"`
	ToolsOriginal           int    `json:"tools_original_estimate,omitempty"`
	ToolsCount              int    `json:"tools_official_count,omitempty"`
	ToolsPadded             int    `json:"tools_padded_estimate,omitempty"`
	ToolsBudgetBefore       int    `json:"tools_message_budget_before,omitempty"`
	ToolsBudgetAfter        int    `json:"tools_message_budget_after,omitempty"`
	ToolsCacheHit           bool   `json:"tools_cache_hit,omitempty"`
	ToolsWallUS             int64  `json:"tools_wall_us,omitempty"`
	ToolsCPUUS              int64  `json:"tools_cpu_us,omitempty"`
	ToolsRecovered          uint64 `json:"tools_worker_recovered_total,omitempty"`
	ToolsRepairFailed       uint64 `json:"tools_worker_repair_failed_total,omitempty"`
	TrimSourceHash          string `json:"trim_source_sha256,omitempty"`
	Kind                    string `json:"kind,omitempty"`
	FragmentPercent         int    `json:"fragment_percent,omitempty"`
	RepeatedPercent         int    `json:"repeated_percent,omitempty"`
	RepeatedEnabled         bool   `json:"repeated_enabled,omitempty"`
	RepeatedAttempted       bool   `json:"repeated_attempted,omitempty"`
	RepeatedMatched         bool   `json:"repeated_matched,omitempty"`
	RepeatedApplied         bool   `json:"repeated_applied,omitempty"`
	Version                 int    `json:"version"`
	Timestamp               string `json:"timestamp"`
	RequestHash             string `json:"request_hash,omitempty"`
	Protocol                string `json:"protocol"`
	Mode                    string `json:"mode"`
	Percent                 int    `json:"percent"`
	Reason                  string `json:"reason"`
	Applied                 bool   `json:"applied"`
	InputBytes              int    `json:"input_bytes"`
	InputItems              int    `json:"input_items"`
	CandidateItems          int    `json:"candidate_items,omitempty"`
	Budget                  int    `json:"budget"`
	SelectMS                int64  `json:"select_ms"`
	Status                  int    `json:"status"`
	DurationMS              int64  `json:"duration_ms"`
	QTerms                  int    `json:"q_terms"`
	CutTerms                int    `json:"cut_terms"`
	BaselineHit             int    `json:"baseline_hit"`
	FinalHit                int    `json:"final_hit"`
	BaselineTailKept        int    `json:"baseline_tail_kept"`
	BudgetUsePct            int    `json:"budget_use_pct"`
	MetricMS                int64  `json:"metric_ms"`
}

func (e *contextRecallEvent) setMetric(m *contextRecallMetric) {
	if m == nil {
		return
	}
	e.QTerms, e.CutTerms, e.BaselineHit, e.FinalHit, e.MetricMS = m.q, m.cut, m.baseHit, m.finalHit, m.ms
	e.BaselineTailKept, e.BudgetUsePct = m.tailKept, m.budgetUse
}

type contextRecallRequestKey struct{}

type contextRecallRequest struct {
	responseIdentity *recallResponseIdentity
	stepToolsMode    string
	stepToolsPercent int
	stepToolsChosen  bool
	stepToolsShadow  func(context.Context)
	stepToolsUntil   time.Time
	stepToolsCancel  context.CancelFunc
	control          *contextRecallControl
	ctx              context.Context
	event            contextRecallEvent
	triggered        bool
	chosen           bool
	fragmentChosen   bool
	repeatedChosen   bool
	disableMetric    bool
}

func newContextRecallControl(path string) *contextRecallControl {
	if path == "" {
		return nil
	}
	c := &contextRecallControl{path: path, engine: newContextRecallEngine()}
	c.emit = func(e contextRecallEvent) {
		if b, err := json.Marshal(e); err == nil {
			fmt.Printf("context_recall %s\n", b)
		}
	}
	return c
}

func (c *contextRecallControl) current() contextRecallPolicy {
	if c == nil {
		return contextRecallPolicy{}
	}
	now := time.Now().UnixNano()
	if now >= c.nextReadNS.Load() && c.readMu.TryLock() {
		defer c.readMu.Unlock()
		if now >= c.nextReadNS.Load() {
			p := contextRecallPolicy{}
			if file, err := os.Open(c.path); err == nil {
				if info, err := file.Stat(); err == nil && info.Mode().IsRegular() && info.Size() <= 1024 {
					b, err := io.ReadAll(io.LimitReader(file, 1025))
					if err != nil || len(b) > 1024 || json.Unmarshal(b, &p) != nil {
						p = contextRecallPolicy{}
					}
				}
				_ = file.Close()
			}
			if (p.Mode != "shadow" && p.Mode != "apply") || p.Percent < 0 || p.Percent > 100 || p.FragmentPercent < 0 || p.FragmentPercent > 100 {
				p = contextRecallPolicy{}
			}
			// An invalid new flag must not disable the existing recall policy.
			if p.RepeatedPercent < 0 || p.RepeatedPercent > 100 {
				p.RepeatedPercent = 0
			}
			if (p.StepToolsMode != "shadow" && p.StepToolsMode != "apply") || p.StepToolsPercent < 0 || p.StepToolsPercent > 100 {
				p.StepToolsMode = ""
				p.StepToolsPercent = 0
			}
			c.policy.Store(&p)
			c.nextReadNS.Store(now + int64(time.Second))
		}
	}
	if p := c.policy.Load(); p != nil {
		return *p
	}
	return contextRecallPolicy{}
}

func (c *contextRecallControl) begin(r *http.Request) *contextRecallRequest {
	p := c.current()
	if p.Percent == 0 && p.FragmentPercent == 0 && p.StepToolsMode == "" {
		return nil
	}
	q := &contextRecallRequest{control: c, ctx: r.Context(), chosen: rand.Intn(100) < p.Percent,
		fragmentChosen: rand.Intn(100) < p.FragmentPercent,
		repeatedChosen: p.RepeatedPercent > 0 && rand.Intn(100) < p.RepeatedPercent,
		event: contextRecallEvent{Version: 1, Mode: p.Mode, Percent: p.Percent, FragmentPercent: p.FragmentPercent, RepeatedPercent: p.RepeatedPercent,
			QTerms: -1, CutTerms: -1, BaselineHit: -1, FinalHit: -1, BaselineTailKept: -1, BudgetUsePct: -1, MetricMS: -1}}
	q.stepToolsMode = p.StepToolsMode
	q.stepToolsPercent = p.StepToolsPercent
	q.stepToolsChosen = p.StepToolsPercent > 0 && rand.Intn(100) < p.StepToolsPercent
	q.event.RepeatedEnabled = q.repeatedChosen
	switch normalizePath(r.URL.Path) {
	case "/messages":
		q.event.Protocol = "messages"
	case "/responses":
		q.event.Protocol = "responses"
	default:
		q.event.Protocol = "chat"
	}
	// Prefer the platform logical ID over an adapter's physical attempt ID.
	id := r.Header.Get("X-Oneapi-Request-Id")
	if id == "" {
		id = r.Header.Get("X-Request-Id")
	}
	if id != "" && len(id) <= 1024 {
		digest := sha256.Sum256([]byte(id))
		q.event.RequestHash = hex.EncodeToString(digest[:16])
	}
	return q
}

func (q *contextRecallRequest) selectItems(payload map[string]any, field string, responses bool, budget int, baseline ...[]any) (kept []any, applied bool) {
	if q == nil {
		return nil, false
	}
	q.triggered = true
	if responses {
		q.event.Protocol = "responses"
	}
	q.event.InputItems = len(payload[field].([]any))
	q.event.Budget = budget
	if !q.chosen {
		q.event.Reason = "control"
		return nil, false
	}
	if len(baseline) != 1 {
		q.event.Reason = "baseline_missing"
		return nil, false
	}
	started := time.Now()
	defer func() {
		q.event.SelectMS = time.Since(started).Milliseconds()
		if recover() != nil {
			kept = nil
			applied = false
			q.event.Applied = false
			q.event.Reason = "internal_fallback"
		}
	}()
	ctx, trace := q.repeatedContext()
	metric := newContextRecallMetric()
	if q.disableMetric {
		metric = nil
	}
	kept, q.event.Reason = q.control.engine.chooseMetric(ctx, payload, q.event.Protocol, q.event.InputBytes, budget, metric, baseline...)
	q.recordRepeated(trace, q.event.Reason == "selected" && q.event.Mode == "apply")
	if q.event.Reason != "selected" {
		metric.finishFinal(true)
		q.event.setMetric(metric)
		return nil, false
	}
	q.event.CandidateItems = len(kept)
	if q.event.Mode != "apply" {
		metric.finishFinal(true)
		q.event.setMetric(metric)
		return nil, false
	}
	metric.finishFinal(false)
	q.event.setMetric(metric)
	q.event.Applied = true
	return kept, true
}

func (q *contextRecallRequest) finish(status int, elapsed time.Duration) {
	if q == nil || !q.triggered {
		return
	}
	q.finishResponseIdentity()
	q.event.Status = status
	q.event.DurationMS = elapsed.Milliseconds()
	q.event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	q.control.emit(q.event)
}
