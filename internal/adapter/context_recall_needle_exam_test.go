package adapter

// Offline acceptance for the question-guided recall repair. Synthetic needle
// exams mirror eval/needle_ab_exam.py: stdlib source filler, a random needle,
// a final question. No model calls. Deterministic seeds.
import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

type needleExamCell struct {
	Size    int
	Pos     float64
	Form    string
	Variant string
	Budget  int
	Needle  string
	Code    string
}

func needleExamCorpus(t *testing.T) string {
	t.Helper()
	var parts []string
	for _, root := range []string{"/usr/lib/python3.13", "/usr/lib/python3"} {
		files, _ := filepath.Glob(filepath.Join(root, "**/*.py"))
		if len(files) == 0 {
			entries, err := os.ReadDir(root)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".py") {
					files = append(files, filepath.Join(root, e.Name()))
				}
			}
		}
		sort.Strings(files)
		for _, path := range files {
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			text := string(data)
			if strings.TrimSpace(text) != "" {
				parts = append(parts, "# ==== "+path+" ====\n"+text)
			}
		}
		if len(parts) > 0 {
			break
		}
	}
	corpus := strings.Join(parts, "")
	if len(corpus) < 2000 {
		t.Fatalf("filler corpus too small: %d", len(corpus))
	}
	// CI images ship only a partial stdlib; retile the collected real text
	// deterministically until the filler reaches exam size.
	for len(corpus) < 4*1000000+200 {
		corpus += "\n# ==== corpus tiling ====\n" + corpus
	}
	return corpus
}

func needleExamCode(rng *rand.Rand) string {
	letters := "ABCDEFGHJKLMNPQRSTUVWXYZ"
	digits := "0123456789"
	var b strings.Builder
	for i := 0; i < 2; i++ {
		b.WriteByte(letters[rng.Intn(len(letters))])
	}
	b.WriteByte(digits[rng.Intn(10)])
	b.WriteByte('-')
	for i := 0; i < 4; i++ {
		b.WriteByte(digits[rng.Intn(10)])
	}
	b.WriteByte('-')
	for i := 0; i < 2; i++ {
		b.WriteByte(letters[rng.Intn(len(letters))])
	}
	return b.String()
}

// buildNeedleExam returns (payload items, needle sentence, question, system).
func buildNeedleExam(corpus string, cell needleExamCell) ([]any, string, string, map[string]any) {
	seed := fmt.Sprintf("%d-%v-%s-%s", cell.Size, cell.Pos, cell.Form, cell.Variant)
	rng := rand.New(rand.NewSource(int64(len(seed))*1000003 + int64(hashString(seed))))
	code := needleExamCode(rng)
	var needle, question string
	if cell.Variant == "en" {
		needle = fmt.Sprintf("\nThe vault code for the warehouse safe is %s.\n", code)
		question = "What is the vault code for the warehouse safe? Answer only the code."
	} else {
		needle = fmt.Sprintf("\n仓库保险柜密码是 %s。\n", code)
		question = "仓库保险柜密码是什么？只回答密码。"
	}
	need := cell.Size * 4
	if len(corpus) < need+200 {
		need = len(corpus) - 200
	}
	start := rng.Intn(len(corpus) - need)
	filler := corpus[start : start+need]
	system := map[string]any{"role": "system", "content": "You are a careful assistant. SYSTEM_KEEP_417."}
	if cell.Form == "single" {
		at := int(float64(len(filler)) * cell.Pos)
		if cut := strings.LastIndex(filler[:at], "\n"); cut >= 0 {
			at = cut
		}
		body := filler[:at] + needle + filler[at:]
		return []any{system, map[string]any{"role": "user", "content": body + "\n\n" + question}}, needle, question, system
	}
	const turns = 11
	step := len(filler) / turns
	target := int(cell.Pos * float64(turns))
	if target > turns-1 {
		target = turns - 1
	}
	var items []any
	items = append(items, system)
	for i := 0; i < turns; i++ {
		end := len(filler)
		if i < turns-1 {
			end = (i + 1) * step
		}
		chunk := filler[i*step : end]
		if i == target {
			mid := strings.LastIndex(chunk[:len(chunk)/2], "\n")
			if mid < 0 {
				mid = 0
			}
			chunk = chunk[:mid] + needle + chunk[mid:]
		}
		items = append(items, map[string]any{"role": "user", "content": chunk})
		items = append(items, map[string]any{"role": "assistant", "content": "收到"})
	}
	items = append(items, map[string]any{"role": "user", "content": question})
	return items, needle, question, system
}

func hashString(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h = (h ^ uint32(s[i])) * 16777619
	}
	return h
}

// examProfile makes contextInputBudget return exactly budget tokens of usable
// message capacity once the request's fixed fields are charged.
func needleExamProfile(payload map[string]any, budget int) contextTrimProfile {
	fixed := 64 + contextTrimOverheadTokens + contextEstimateAnyTokens(payload["system"])
	return contextTrimProfile{ContextLimit: budget + fixed}
}

type needleExamOutcome struct {
	contains   bool
	questionOK bool
	est        int
	reason     string
	selectMS   int64
}

func runNeedleExam(items []any, budget int, recall bool) (needleExamOutcome, string, []any) {
	payload := map[string]any{"model": "exam/model", "max_tokens": 64, "messages": cloneContextJSONValue(items)}
	profile := needleExamProfile(payload, budget)
	var stats contextTrimStats
	var q *contextRecallRequest
	if recall {
		engine := newContextRecallEngine()
		engine.timeout = 5 * time.Second // isolate selection cost from the production deadline for measurement
		q = &contextRecallRequest{control: &contextRecallControl{engine: engine}, ctx: context.Background(), chosen: true, fragmentChosen: true, event: contextRecallEvent{Mode: "apply", Protocol: "chat", Budget: budget}}
	}
	trimMessagePayload(payload, "messages", profile, false, &stats, q)
	out := needleExamOutcome{est: contextEstimateItemsTokens(payload["messages"].([]any))}
	if q != nil {
		out.reason = q.event.Reason
		out.selectMS = q.event.SelectMS
	}
	var text strings.Builder
	for _, item := range payload["messages"].([]any) {
		text.WriteString(contextRecallText(item))
	}
	return out, text.String(), payload["messages"].([]any)
}

func TestContextRecallNeedleExam(t *testing.T) {
	corpus := needleExamCorpus(t)
	var cells []needleExamCell
	for _, size := range []int{200000, 500000, 1000000} {
		for _, pos := range []float64{0.10, 0.50, 0.90} {
			for _, form := range []string{"single", "multi"} {
				for _, variant := range []string{"zh", "en"} {
					for _, budget := range []int{220000, 91376} {
						cells = append(cells, needleExamCell{Size: size, Pos: pos, Form: form, Variant: variant, Budget: budget})
					}
				}
			}
		}
	}
	type tally struct{ hit, total int }
	forms := map[string]*tally{"single": {}, "multi": {}}
	before := map[string]*tally{"single": {}, "multi": {}}
	var maxSelect int64
	var rows []string
	for _, cell := range cells {
		items, needle, question, system := buildNeedleExam(corpus, cell)
		oldOut, oldText, _ := runNeedleExam(items, cell.Budget, false)
		newOut, text, kept := runNeedleExam(items, cell.Budget, true)
		newOut.contains = strings.Contains(text, strings.TrimSpace(needle))
		newOut.questionOK = strings.Contains(text, question)
		oldOut.contains = strings.Contains(oldText, strings.TrimSpace(needle))
		label := fmt.Sprintf("size=%dk pos=%.0f form=%s %s budget=%d", cell.Size/1000, cell.Pos*100, cell.Form, cell.Variant, cell.Budget)
		rows = append(rows, fmt.Sprintf("%-58s before=%v after=%v reason=%s est=%d q=%v ms=%d", label, oldOut.contains, newOut.contains, newOut.reason, newOut.est, newOut.questionOK, newOut.selectMS))
		if oldOut.contains {
			before[cell.Form].hit++
		}
		before[cell.Form].total++
		if newOut.contains {
			forms[cell.Form].hit++
		}
		forms[cell.Form].total++
		if newOut.selectMS > maxSelect {
			maxSelect = newOut.selectMS
		}
		// Per-cell invariants.
		if !newOut.questionOK {
			t.Errorf("%s: question not fully preserved", label)
		}
		if newOut.est > cell.Budget {
			t.Errorf("%s: estimate %d exceeds budget %d", label, newOut.est, cell.Budget)
		}
		sysOK := false
		for _, item := range kept {
			if m, ok := item.(map[string]any); ok && m["role"] == "system" && m["content"] == system["content"] {
				sysOK = true
			}
		}
		if !sysOK {
			t.Errorf("%s: system message changed", label)
		}
	}
	for _, row := range rows {
		t.Log(row)
	}
	for form, tly := range forms {
		rate := float64(tly.hit) / float64(tly.total)
		want := 0.95
		if form == "multi" {
			want = 0.90
		}
		t.Logf("form=%s needle=%d/%d (before %d/%d) rate=%.3f want=%.2f", form, tly.hit, tly.total, before[form].hit, before[form].total, rate, want)
		if rate < want {
			t.Errorf("form=%s needle rate %.3f below %.2f", form, rate, want)
		}
	}
	t.Logf("max select_ms=%d", maxSelect)
	var encoded strings.Builder
	for _, row := range rows {
		encoded.WriteString(row + "\n")
	}
	_ = encoded
}

// A pasted log ends with a unique error and the question shares no words with
// it. The recall must still keep the document ending right before the
// question, and the plain baseline keeps it too as a control.
func TestContextRecallLogTailPreserved(t *testing.T) {
	const want = "FATAL: shard-7 checksum mismatch at offset 99812"
	line := "2026-10-04T03:12:11Z INFO worker-3 processed batch 4815 latency=23ms ok\n"
	filler := strings.Repeat(line, 1000000/len(line)+1)
	var doc strings.Builder
	decoys := []float64{0.30, 0.42, 0.55, 0.66, 0.78}
	di := 0
	for pos := 0; pos+20000 <= len(filler); pos += 20000 {
		doc.WriteString(filler[pos : pos+20000])
		if di < len(decoys) && float64(pos+20000)/float64(len(filler)) >= decoys[di] {
			doc.WriteString("2026-10-04T03:12:11Z NOTICE transient 报错 recovered after retry\n")
			di++
		}
	}
	doc.WriteString("2026-10-04T03:59:58Z " + want + "\n")
	doc.WriteString(filler[:1500])
	question := "这个报错怎么修？"
	body := doc.String() + "\n" + question + "\n"
	if contextEstimateStringTokens(body) < 480000 {
		t.Fatalf("log fixture too small: %d est", contextEstimateStringTokens(body))
	}
	system := map[string]any{"role": "system", "content": "You are a careful assistant."}
	items := []any{system, map[string]any{"role": "user", "content": body}}
	for _, budget := range []int{220000, 91376} {
		oldOut, oldText, _ := runNeedleExam(items, budget, false)
		if !strings.Contains(oldText, want) {
			t.Fatalf("budget=%d baseline lost the error line (control broken)", budget)
		}
		newOut, text, _ := runNeedleExam(items, budget, true)
		t.Logf("budget=%d reason=%s est=%d baseline_est=%d", budget, newOut.reason, newOut.est, oldOut.est)
		if newOut.reason != "fragment_selected" {
			t.Fatalf("budget=%d recall did not apply: %s", budget, newOut.reason)
		}
		if !strings.Contains(text, want) {
			t.Fatalf("budget=%d error line dropped despite preserved tail", budget)
		}
		if !strings.Contains(text, question) {
			t.Fatalf("budget=%d question not preserved", budget)
		}
		if newOut.est > budget {
			t.Fatalf("budget=%d estimate %d exceeds budget", budget, newOut.est)
		}
	}
}
