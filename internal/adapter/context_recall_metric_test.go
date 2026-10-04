package adapter

// Offline coverage for the cut-content recall metric: counts are numbers
// only, reuse the selector's own term tables, and never change the selected
// output. Deterministic inputs. No model calls.
import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func metricRecallRequest(engine *contextRecallEngine, protocol string, budget int) *contextRecallRequest {
	return &contextRecallRequest{
		control:        &contextRecallControl{engine: engine},
		ctx:            context.Background(),
		chosen:         true,
		fragmentChosen: true,
		event:          contextRecallEvent{Mode: "apply", Protocol: protocol, Budget: budget},
	}
}

func runMetricExam(items []any, budget int, disableMetric bool) (*contextRecallRequest, map[string]any) {
	payload := map[string]any{"model": "exam/model", "max_tokens": 64, "messages": cloneContextJSONValue(items)}
	profile := needleExamProfile(payload, budget)
	var stats contextTrimStats
	engine := newContextRecallEngine()
	engine.timeout = 5 * time.Second
	q := metricRecallRequest(engine, "chat", budget)
	q.disableMetric = disableMetric
	trimMessagePayload(payload, "messages", profile, false, &stats, q)
	return q, payload
}

// The single-document exam repairs a truncated pasted document through the
// fragment path: the question shares terms only with the cut needle region.
func TestContextRecallMetricSingleNeedle(t *testing.T) {
	corpus := needleExamCorpus(t)
	for _, variant := range []string{"zh", "en"} {
		items, needle, question, _ := buildNeedleExam(corpus, needleExamCell{Size: 500000, Pos: 0.5, Form: "single", Variant: variant, Budget: 91376})
		q, payload := runMetricExam(items, 91376, false)
		ev := q.event
		text := contextRecallJoinedText(payload["messages"].([]any))
		if ev.Reason != "fragment_selected" || !strings.Contains(text, strings.TrimSpace(needle)) {
			t.Fatalf("%s: recall did not restore the needle: reason=%s", variant, ev.Reason)
		}
		if !strings.Contains(text, question) {
			t.Fatalf("%s: question not preserved", variant)
		}
		if ev.QTerms <= 0 {
			t.Fatalf("%s: q_terms=%d want >0", variant, ev.QTerms)
		}
		if ev.CutTerms <= 0 {
			t.Fatalf("%s: cut_terms=%d want >0 (needle terms were cut)", variant, ev.CutTerms)
		}
		if ev.BaselineHit != 0 {
			t.Fatalf("%s: baseline_hit=%d want 0", variant, ev.BaselineHit)
		}
		if ev.FinalHit == -1 {
			// Documented deadline fallback: under deadline pressure fields
			// stay -1 rather than slowing the request.
			if ev.MetricMS <= 0 {
				t.Fatalf("%s: final_hit=-1 without deadline pressure", variant)
			}
			t.Logf("%s single: metric hit its time budget, final_hit=-1 (ms=%d)", variant, ev.MetricMS)
		} else if ev.FinalHit != ev.CutTerms {
			t.Fatalf("%s: final_hit=%d cut_terms=%d", variant, ev.FinalHit, ev.CutTerms)
		}
		if ev.MetricMS < 0 || ev.MetricMS > 80 {
			t.Fatalf("%s: metric_ms=%d out of bound", variant, ev.MetricMS)
		}
		t.Logf("%s single: q=%d cut=%d base=%d final=%d metric_ms=%d select_ms=%d", variant, ev.QTerms, ev.CutTerms, ev.BaselineHit, ev.FinalHit, ev.MetricMS, ev.SelectMS)
	}
}

// The multi-turn exam drops middle history items and takes the item path:
// needle terms occur once in the question and once in the cut turn.
func TestContextRecallMetricMultiNeedle(t *testing.T) {
	corpus := needleExamCorpus(t)
	for _, variant := range []string{"zh", "en"} {
		items, needle, question, _ := buildNeedleExam(corpus, needleExamCell{Size: 500000, Pos: 0.5, Form: "multi", Variant: variant, Budget: 91376})
		q, payload := runMetricExam(items, 91376, false)
		ev := q.event
		text := contextRecallJoinedText(payload["messages"].([]any))
		if ev.Reason != "selected" || !strings.Contains(text, strings.TrimSpace(needle)) {
			t.Fatalf("%s: recall did not restore the needle: reason=%s", variant, ev.Reason)
		}
		if !strings.Contains(text, question) {
			t.Fatalf("%s: question not preserved", variant)
		}
		if ev.QTerms <= 0 || ev.CutTerms <= 0 {
			t.Fatalf("%s: q_terms=%d cut_terms=%d want both >0", variant, ev.QTerms, ev.CutTerms)
		}
		if ev.BaselineHit != 0 {
			t.Fatalf("%s: baseline_hit=%d want 0", variant, ev.BaselineHit)
		}
		if ev.FinalHit == -1 {
			if ev.MetricMS <= 0 {
				t.Fatalf("%s: final_hit=-1 without deadline pressure", variant)
			}
			t.Logf("%s multi: metric hit its time budget, final_hit=-1 (ms=%d)", variant, ev.MetricMS)
		} else if ev.FinalHit != ev.CutTerms {
			t.Fatalf("%s: final_hit=%d cut_terms=%d", variant, ev.FinalHit, ev.CutTerms)
		}
		if ev.MetricMS < 0 || ev.MetricMS > 80 {
			t.Fatalf("%s: metric_ms=%d out of bound", variant, ev.MetricMS)
		}
		t.Logf("%s multi: q=%d cut=%d base=%d final=%d metric_ms=%d select_ms=%d", variant, ev.QTerms, ev.CutTerms, ev.BaselineHit, ev.FinalHit, ev.MetricMS, ev.SelectMS)
	}
}

// When every question term with outside occurrences still lives in the kept
// recent region, nothing the question references was cut: cut_terms == 0.
func TestContextRecallMetricRetainedQuestion(t *testing.T) {
	var items []any
	items = append(items, map[string]any{"role": "system", "content": "You are a careful assistant."})
	for n := 0; n < 30; n++ {
		items = append(items,
			map[string]any{"role": "user", "content": "archive entry " + strings.Repeat("routine sampling notes ", 30)},
			map[string]any{"role": "assistant", "content": "recorded"})
	}
	items = append(items,
		map[string]any{"role": "user", "content": "仓库保险柜密码是 AB1-2345-XY，登记在第 12 页。"},
		map[string]any{"role": "assistant", "content": "已记录。"},
		map[string]any{"role": "user", "content": "仓库保险柜密码是什么？只回答密码。"})
	const budget = 6000
	q, payload := runMetricExam(items, budget, false)
	ev := q.event
	if ev.QTerms <= 0 {
		t.Fatalf("q_terms=%d want >0", ev.QTerms)
	}
	if ev.CutTerms != 0 {
		t.Fatalf("cut_terms=%d want 0: question terms survive in the kept region (reason=%s)", ev.CutTerms, ev.Reason)
	}
	if ev.BaselineHit != 0 || ev.FinalHit != 0 {
		t.Fatalf("baseline_hit=%d final_hit=%d want 0", ev.BaselineHit, ev.FinalHit)
	}
	kept := payload["messages"].([]any)
	found := false
	for _, item := range kept {
		if m, ok := item.(map[string]any); ok && strings.Contains(fmt.Sprint(m["content"]), "AB1-2345-XY") {
			found = true
		}
	}
	if !found {
		t.Fatal("fixture invalid: needle turn was not in the retained region")
	}
	t.Logf("retained: q=%d cut=%d reason=%s metric_ms=%d", ev.QTerms, ev.CutTerms, ev.Reason, ev.MetricMS)
}

// Identical inputs with the metric on or off must produce byte-identical
// output on both the item and the fragment paths.
func TestContextRecallMetricOutputIdentical(t *testing.T) {
	corpus := needleExamCorpus(t)
	cells := []needleExamCell{
		{Size: 300000, Pos: 0.5, Form: "single", Variant: "zh", Budget: 91376},
		{Size: 300000, Pos: 0.5, Form: "multi", Variant: "zh", Budget: 91376},
		{Size: 300000, Pos: 0.9, Form: "multi", Variant: "en", Budget: 220000},
	}
	for _, cell := range cells {
		items, _, _, _ := buildNeedleExam(corpus, cell)
		qOn, pOn := runMetricExam(items, cell.Budget, false)
		qOff, pOff := runMetricExam(items, cell.Budget, true)
		on := mustJSON(t, pOn["messages"])
		off := mustJSON(t, pOff["messages"])
		if string(on) != string(off) {
			t.Fatalf("%s/%s/%.1f: metric changed the output", cell.Form, cell.Variant, cell.Pos)
		}
		if qOn.event.Reason != qOff.event.Reason || qOn.event.Applied != qOff.event.Applied {
			t.Fatalf("%s/%s/%.1f: decision changed on=%s off=%s", cell.Form, cell.Variant, cell.Pos, qOn.event.Reason, qOff.event.Reason)
		}
	}
}

// Measure the real metric cost on a ~4MB single-document request, which is
// the heaviest path (fragment recall plus baseline/final term counts).
func TestContextRecallMetricScaleCost(t *testing.T) {
	corpus := needleExamCorpus(t)
	items, _, _, _ := buildNeedleExam(corpus, needleExamCell{Size: 1000000, Pos: 0.5, Form: "single", Variant: "zh", Budget: 91376})
	inputBytes := len(mustJSON(t, map[string]any{"messages": items}))
	q, _ := runMetricExam(items, 91376, false)
	ev := q.event
	t.Logf("1M single: bytes=%d reason=%s q=%d cut=%d base=%d final=%d metric_ms=%d select_ms=%d",
		inputBytes, ev.Reason, ev.QTerms, ev.CutTerms, ev.BaselineHit, ev.FinalHit, ev.MetricMS, ev.SelectMS)
	if ev.MetricMS < 0 {
		t.Fatalf("metric_ms=%d want >=0", ev.MetricMS)
	}
	if ev.MetricMS > 80 {
		t.Fatalf("metric_ms=%d exceeds the bounded budget", ev.MetricMS)
	}
}
