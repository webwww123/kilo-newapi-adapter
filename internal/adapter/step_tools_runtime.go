package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

type contextStepToolsGuardKey struct{}

func (c *contextRecallControl) startStepTools(bundle string) {
	if c == nil || bundle == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p, err := newStepToolCounterPool(ctx, []string{"/usr/bin/python3", "-I", "-S", "-B", filepath.Join(bundle, "worker", "step_tool_counter_worker.py"), "--assets", filepath.Join(bundle, "assets")})
	if err != nil {
		fmt.Println("step_tools_worker {\"ready\":false,\"reason\":\"startup_failed\"}")
		return
	}
	c.stepTools = p
	fmt.Println("step_tools_worker {\"ready\":true,\"revision\":\"" + stepCounterRevision + "\"}")
}

func (c *contextRecallControl) closeStepTools() {
	if c != nil {
		c.stepTools.Close()
	}
}

// A shadow count runs after the legacy selection, so counting cannot consume
// the selector's deadline or change the body. Its remaining time is bounded.
func contextStepToolsTrimWork(payload map[string]any, recall ...*contextRecallRequest) func() {
	if len(recall) == 0 || recall[0] == nil {
		return func() {}
	}
	q := recall[0]
	model, _ := payload["model"].(string)
	if q.stepToolsMode == "" || q.ctx == nil || q.event.Protocol != "chat" || strings.TrimPrefix(model, "kilo/") != "stepfun/step-3.7-flash:free" || contextEstimateAnyTokens(payload["tools"]) < 8192 {
		return func() {}
	}
	q.stepToolsUntil = time.Now().Add(250 * time.Millisecond)
	previous := q.ctx
	return func() {
		if q.stepToolsShadow != nil {
			ctx, stop := context.WithDeadline(previous, q.stepToolsUntil)
			q.stepToolsShadow(ctx)
			stop()
			q.stepToolsShadow = nil
		}
		if q.stepToolsCancel != nil {
			q.stepToolsCancel()
			q.stepToolsCancel = nil
			q.ctx = previous
		}
	}
}

func contextStepToolsTokens(payload map[string]any, profile contextTrimProfile, withoutTools int, recall ...*contextRecallRequest) int {
	original := contextEstimateAnyTokens(payload["tools"])
	if len(recall) == 0 || recall[0] == nil {
		return original
	}
	q := recall[0]
	model, _ := payload["model"].(string)
	items, ok := payload["messages"].([]any)
	if q.stepToolsMode == "" || q.ctx == nil || q.event.Protocol != "chat" || strings.TrimPrefix(model, "kilo/") != "stepfun/step-3.7-flash:free" || original < 8192 || !ok {
		return original
	}
	budget := withoutTools - original
	if profile.MaxInputTokens > 0 && budget > profile.MaxInputTokens {
		budget = profile.MaxInputTokens
	}
	if contextEstimateItemsTokens(items) <= budget {
		return original
	}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok || item["thinking"] != nil || item["signature"] != nil || item["function_call"] != nil {
			return original
		}
		switch item["role"] {
		case "system", "user", "assistant", "tool":
		default:
			return original
		}
		switch content := item["content"].(type) {
		case nil, string:
		case []any:
			for _, raw := range content {
				block, ok := raw.(map[string]any)
				if !ok || block["type"] != "text" {
					return original
				}
			}
		default:
			return original
		}
	}
	q.triggered = true
	q.event.ToolsEligible = true
	q.event.ToolsMode = q.stepToolsMode
	q.event.ToolsPercent = q.stepToolsPercent
	q.event.ToolsOriginal = original
	q.event.ToolsBudgetBefore = budget
	q.event.ToolsBudgetAfter = budget
	q.event.ToolsModel = model
	// Exact canonical bytes, never a time-based join or a customer-text log.
	if b, err := json.Marshal(payload); err == nil {
		sum := sha256.Sum256(b)
		q.event.TrimSourceHash = hex.EncodeToString(sum[:])
	}
	if !q.stepToolsChosen {
		q.event.ToolsReason = "not_sampled"
		return original
	}
	if profile.MaxInputTokens > 0 && budget == profile.MaxInputTokens {
		q.event.ToolsReason = "input_cap_no_gain"
		return original
	}
	if q.control.stepTools == nil {
		q.event.ToolsReason = "worker_unavailable"
		return original
	}
	count := func(ctx context.Context) int {
		started := time.Now()
		q.event.ToolsAttempted = true
		result, err := q.control.stepTools.Count(ctx, payload["tools"])
		q.event.ToolsWallUS = time.Since(started).Microseconds()
		q.event.ToolsRecovered = q.control.stepTools.recovered.Load()
		q.event.ToolsRepairFailed = q.control.stepTools.repairFailed.Load()
		if err != nil {
			q.event.ToolsReason = err.Error()
			if ctx.Err() == context.Canceled {
				q.event.ToolsReason = "client_cancelled"
			}
			if ctx.Err() == context.DeadlineExceeded {
				q.event.ToolsReason = "processing_deadline"
			}
			return original
		}
		q.event.ToolsCacheHit = result.CacheHit
		q.event.ToolsCount = result.Tokens
		q.event.ToolsCPUUS = result.CPUUS
		padded := (result.Tokens*11+9)/10 + 4096
		if padded >= original {
			q.event.ToolsReason = "no_budget_gain"
			return original
		}
		q.event.ToolsPadded = padded
		newBudget := withoutTools - padded
		if profile.MaxInputTokens > 0 && newBudget > profile.MaxInputTokens {
			newBudget = profile.MaxInputTokens
		}
		q.event.ToolsBudgetAfter = newBudget
		if q.stepToolsMode == "shadow" {
			q.event.ToolsReason = "shadow_counted"
			return original
		}
		q.event.ToolsReason = "budget_applied"
		q.event.ToolsApplied = true
		return padded
	}
	if q.stepToolsMode == "shadow" {
		q.stepToolsShadow = func(ctx context.Context) { _ = count(ctx) }
		return original
	}
	if !q.stepToolsUntil.IsZero() {
		q.ctx, q.stepToolsCancel = context.WithDeadline(q.ctx, q.stepToolsUntil)
	}
	return count(q.ctx)
}
