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
	Mode    string `json:"mode"`
	Percent int    `json:"percent"`
}

type contextRecallControl struct {
	path       string
	engine     *contextRecallEngine
	policy     atomic.Pointer[contextRecallPolicy]
	nextReadNS atomic.Int64
	readMu     sync.Mutex
	emit       func(contextRecallEvent)
}

type contextRecallEvent struct {
	Version        int    `json:"version"`
	Timestamp      string `json:"timestamp"`
	RequestHash    string `json:"request_hash,omitempty"`
	Protocol       string `json:"protocol"`
	Mode           string `json:"mode"`
	Percent        int    `json:"percent"`
	Reason         string `json:"reason"`
	Applied        bool   `json:"applied"`
	InputBytes     int    `json:"input_bytes"`
	InputItems     int    `json:"input_items"`
	CandidateItems int    `json:"candidate_items,omitempty"`
	Budget         int    `json:"budget"`
	SelectMS       int64  `json:"select_ms"`
	Status         int    `json:"status"`
	DurationMS     int64  `json:"duration_ms"`
}

type contextRecallRequestKey struct{}

type contextRecallRequest struct {
	control   *contextRecallControl
	ctx       context.Context
	event     contextRecallEvent
	triggered bool
	chosen    bool
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
			if (p.Mode != "shadow" && p.Mode != "apply") || p.Percent < 0 || p.Percent > 100 {
				p = contextRecallPolicy{}
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
	if p.Percent == 0 {
		return nil
	}
	q := &contextRecallRequest{control: c, ctx: r.Context(), chosen: rand.Intn(100) < p.Percent,
		event: contextRecallEvent{Version: 1, Mode: p.Mode, Percent: p.Percent}}
	switch normalizePath(r.URL.Path) {
	case "/messages":
		q.event.Protocol = "messages"
	case "/responses":
		q.event.Protocol = "responses"
	default:
		q.event.Protocol = "chat"
	}
	if id := r.Header.Get("X-Request-Id"); id != "" && len(id) <= 1024 {
		digest := sha256.Sum256([]byte(id))
		q.event.RequestHash = hex.EncodeToString(digest[:16])
	}
	return q
}

func (q *contextRecallRequest) selectItems(payload map[string]any, field string, responses bool, budget int) (kept []any, applied bool) {
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
	kept, q.event.Reason = q.control.engine.choose(q.ctx, payload, q.event.Protocol, q.event.InputBytes, budget)
	if q.event.Reason != "selected" {
		return nil, false
	}
	q.event.CandidateItems = len(kept)
	if q.event.Mode != "apply" {
		return nil, false
	}
	q.event.Applied = true
	return kept, true
}

func (q *contextRecallRequest) finish(status int, elapsed time.Duration) {
	if q == nil || !q.triggered {
		return
	}
	q.event.Status = status
	q.event.DurationMS = elapsed.Milliseconds()
	q.event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	q.control.emit(q.event)
}
