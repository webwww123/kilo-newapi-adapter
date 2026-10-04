package adapter

// Large-session metric coverage: a 2500-item ~3.4MB session and a 16000-item
// ~4MB session on both wire protocols. These sizes used to leave every metric
// field at -1 because the whole baseline was rescanned per question term; the
// drop-region subtraction must still produce identical counts, fast.
import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// bigSession builds item-count messages averaging targetBytes overall with a
// single needle sentence inside an old user turn and a final question that
// shares the needle's rare terms.
func bigSessionItems(count, targetBytes int, needle, question string) []any {
	fillerLine := "observatory journal calibration wind direction cloud coverage archive rotation routine inventory sensor reading telemetry packet checksum margin note "
	items := []any{map[string]any{"role": "system", "content": "You are a careful assistant. SYSTEM_KEEP_417."}}
	per := targetBytes / count
	if per < 200 {
		per = 200
	}
	chunk := strings.Repeat(fillerLine, per/len(fillerLine)+1)[:per]
	needleAt := count / 3
	for i := 0; i < count-2; i++ {
		var role string
		if i%2 == 0 {
			role = "user"
		} else {
			role = "assistant"
		}
		text := fmt.Sprintf("record %05d %s", i, chunk)
		if i == needleAt {
			text = needle + "\n" + text
		}
		items = append(items, map[string]any{"role": role, "content": text})
	}
	items = append(items, map[string]any{"role": "user", "content": question})
	return items
}

// bigSessionRender mirrors the fixture renderer without the offline build tag:
// chat keeps OpenAI shapes; messages converts to Anthropic envelopes.
func bigSessionRender(chatItems []any, protocol string) map[string]any {
	payload := map[string]any{"model": "exam/model", "max_tokens": 64}
	if protocol != "messages" {
		payload["messages"] = chatItems
		return payload
	}
	var system []string
	var items []any
	for _, raw := range chatItems {
		m := raw.(map[string]any)
		if m["role"] == "system" {
			system = append(system, m["content"].(string))
			continue
		}
		items = append(items, m)
	}
	payload["system"] = strings.Join(system, "\n")
	payload["messages"] = items
	return payload
}

// runBigMetric drives the same calls selectItems performs so the metric's
// internal state stays observable after the request completes.
func runBigMetric(t *testing.T, engine *contextRecallEngine, payload map[string]any, protocol string, budget int) (metric *contextRecallMetric, reason string, selectMS int64) {
	field := contextRecallField(protocol)
	profile := needleExamProfile(payload, budget)
	var stats contextTrimStats
	available := contextInputBudget(payload, profile, false, &stats)
	working, _ := cloneContextJSONValue(payload[field]).([]any)
	baseline := selectContextItemsForBudget(working, available)
	raw := len(mustJSON(t, payload[field]))
	metric = newContextRecallMetric()
	ctx := context.Background()
	start := time.Now()
	_, reason = engine.chooseMetric(ctx, payload, protocol, raw, available, metric, baseline)
	selectMS = time.Since(start).Milliseconds()
	metric.finishFinal(reason != "selected")
	return metric, reason, selectMS
}

func TestContextRecallMetricLargeSession(t *testing.T) {
	const needle = "\n仓库保险柜密码是 ZX9-4321-AB；门禁备份码 KL7-8854-CD；机柜钥匙 QW2-1163-EF；磁带库标签 RT5-7721-GH；保险丝编号 MN8-3390-JK；冷备电源口令 BV4-5562-QP。\n"
	const question = "仓库保险柜密码、门禁备份码、机柜钥匙、磁带库标签、保险丝编号、冷备电源口令分别是什么？逐项只回答编号。"
	for _, cell := range []struct {
		count, bytes, budget int
	}{
		{2500, 3400 * 1024, 800000},
		{16000, 4 * 1024 * 1024, 900000},
	} {
		for _, protocol := range []string{"chat", "messages"} {
			items := bigSessionItems(cell.count, cell.bytes, needle, question)
			payload := bigSessionRender(items, protocol)
			engine := newContextRecallEngine()
			m, reason, sel := runBigMetric(t, engine, payload, protocol, cell.budget)
			t.Logf("large %dx%dKB %s: reason=%s select_ms=%d q=%d cut=%d base=%d final=%d metric_ms=%d",
				cell.count, cell.bytes/1024, protocol, reason, sel, m.q, m.cut, m.baseHit, m.finalHit, m.ms)
			if reason != "selected" {
				t.Fatalf("%d/%s: recall did not apply: %s", cell.count, protocol, reason)
			}
			if m.q <= 0 || m.cut <= 0 || m.finalHit != m.cut {
				t.Fatalf("%d/%s: q=%d cut=%d final=%d want computed metric", cell.count, protocol, m.q, m.cut, m.finalHit)
			}
			if m.baseHit != 0 {
				t.Fatalf("%d/%s: baseline_hit=%d want 0", cell.count, protocol, m.baseHit)
			}
			if m.ms < 0 || m.ms > 80 {
				t.Fatalf("%d/%s: metric_ms=%d out of bound", cell.count, protocol, m.ms)
			}
		}
	}
}
