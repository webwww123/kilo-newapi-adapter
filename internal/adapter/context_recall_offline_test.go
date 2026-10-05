//go:build recall_offline

package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type recallMeasurement struct {
	WallMS      float64 `json:"wall_ms"`
	CPUMS       float64 `json:"cpu_ms"`
	AllocatedMB float64 `json:"allocated_mib"`
}

type recallCaseResult struct {
	Scenario       string            `json:"scenario"`
	Protocol       string            `json:"protocol"`
	InputTarget    int               `json:"input_target"`
	InputEstimate  int               `json:"input_estimate"`
	InputBytes     int               `json:"input_bytes"`
	Budget         int               `json:"usable_input_budget"`
	Expected       int               `json:"required_evidence"`
	OldHits        int               `json:"old_hits"`
	NewHits        int               `json:"new_hits"`
	Gained         []string          `json:"gained_evidence"`
	Lost           []string          `json:"lost_evidence"`
	OldEstimate    int               `json:"old_output_estimate"`
	NewEstimate    int               `json:"new_output_estimate"`
	Reason         string            `json:"candidate_reason"`
	OldMeasurement recallMeasurement `json:"baseline"`
	NewMeasurement recallMeasurement `json:"candidate"`
}

func recallCPU() float64 {
	var r syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &r)
	return float64(r.Utime.Sec+r.Stime.Sec)*1000 + float64(r.Utime.Usec+r.Stime.Usec)/1000
}

func measuredRecall(t *testing.T, raw []byte, protocol string, profile contextTrimProfile, candidate *recallExperiment) ([]byte, string, recallMeasurement) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	cpu := recallCPU()
	start := time.Now()
	out, reason, err := runRecallExperiment(context.Background(), raw, protocol, profile, candidate)
	wall := float64(time.Since(start).Nanoseconds()) / 1e6
	cpu = recallCPU() - cpu
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	return out, reason, recallMeasurement{WallMS: wall, CPUMS: cpu, AllocatedMB: float64(after.TotalAlloc-before.TotalAlloc) / (1 << 20)}
}

func decodeRecall(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func recallJSON(t *testing.T, value any) []byte {
	t.Helper()
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func validateRecallSelection(t *testing.T, original, output []byte, protocol string, budget int, selected bool) int {
	t.Helper()
	in, out := decodeRecall(t, original), decodeRecall(t, output)
	field := recallField(protocol)
	before, after := in[field].([]any), out[field].([]any)
	delete(in, field)
	delete(out, field)
	if !reflect.DeepEqual(in, out) {
		t.Fatal("fixed request fields changed")
	}
	if !contextToolHistorySafeToTrim(after) || (protocol == "responses" && !contextResponsesItemsSafeToTrim(after)) {
		t.Fatal("tool pairing was broken")
	}
	estimate := contextEstimateItemsTokens(after)
	if estimate > budget {
		t.Fatalf("output estimate %d exceeds budget %d", estimate, budget)
	}
	if selected {
		// Independently verify exact, ordered original items; no rewritten text.
		at := 0
		for _, item := range after {
			wanted := recallJSON(t, item)
			for at < len(before) && !bytes.Equal(recallJSON(t, before[at]), wanted) {
				at++
			}
			if at == len(before) {
				t.Fatal("candidate rewrote or reordered an item")
			}
			at++
		}
	}
	for _, anchor := range []string{"SYSTEM_POLICY_731", "DEVELOPER_POLICY_619", "CURRENT_TASK_927", "CURRENT_TOOL_RESULT_518", "call_current"} {
		if !bytes.Contains(output, []byte(anchor)) {
			t.Fatalf("required active context missing: %s", anchor)
		}
	}
	return estimate
}

func TestRecallOfflineComparison(t *testing.T) {
	path := os.Getenv("RECALL_OFFLINE_REPORT")
	if path == "" {
		t.Skip("set RECALL_OFFLINE_REPORT for this explicit offline experiment")
	}
	var results []recallCaseResult
	settings := []struct{ input, budget int }{{500000, 128000}, {1000000, 128000}, {1000000, 256000}}
	for _, scenario := range recallScenarios {
		for _, protocol := range []string{"chat", "messages", "responses"} {
			for _, setting := range settings {
				name := fmt.Sprintf("%s/%s/%d_to_%d", scenario, protocol, setting.input, setting.budget)
				t.Run(name, func(t *testing.T) {
					fixture := makeRecallFixture(scenario, protocol, setting.input)
					profile := recallProfile(fixture.Payload, protocol, setting.budget)
					raw := recallJSON(t, fixture.Payload)
					originalHash := sha256.Sum256(raw)
					r := recallCaseResult{Scenario: scenario, Protocol: protocol, InputTarget: setting.input, Budget: setting.budget, InputBytes: len(raw), Expected: len(fixture.Evidence)}
					r.InputEstimate = contextEstimateItemsTokens(fixture.Payload[recallField(protocol)].([]any))
					var old, candidate []byte
					// Alternate order across cases to avoid always warming one arm.
					if len(results)%2 == 0 {
						old, _, r.OldMeasurement = measuredRecall(t, raw, protocol, profile, nil)
						candidate, r.Reason, r.NewMeasurement = measuredRecall(t, raw, protocol, profile, newRecallExperiment())
					} else {
						candidate, r.Reason, r.NewMeasurement = measuredRecall(t, raw, protocol, profile, newRecallExperiment())
						old, _, r.OldMeasurement = measuredRecall(t, raw, protocol, profile, nil)
					}
					r.OldEstimate = validateRecallSelection(t, raw, old, protocol, setting.budget, false)
					r.NewEstimate = validateRecallSelection(t, raw, candidate, protocol, setting.budget, r.Reason == "selected")
					oldText := recallText(decodeRecall(t, old)[recallField(protocol)])
					newText := recallText(decodeRecall(t, candidate)[recallField(protocol)])
					for _, fact := range fixture.Evidence {
						a, b := strings.Contains(oldText, fact), strings.Contains(newText, fact)
						if a {
							r.OldHits++
						}
						if b {
							r.NewHits++
						}
						if b && !a {
							r.Gained = append(r.Gained, fact)
						}
						if a && !b {
							r.Lost = append(r.Lost, fact)
						}
					}
					if sha256.Sum256(raw) != originalHash {
						t.Fatal("original input mutated")
					}
					if r.Reason != "selected" && !bytes.Equal(old, candidate) {
						t.Fatal("fallback differs from original trimmer")
					}
					results = append(results, r)
					t.Logf("evidence old=%d/%d new=%d/%d reason=%s wall old=%.1fms new=%.1fms", r.OldHits, r.Expected, r.NewHits, r.Expected, r.Reason, r.OldMeasurement.WallMS, r.NewMeasurement.WallMS)
				})
			}
		}
	}
	fixtureSource, err := os.ReadFile("context_recall_fixtures_offline_test.go")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(fixtureSource)
	report := map[string]any{
		"baseline_revision": "033517b6fed09378252968e6569674219f41cc8f",
		"fixture_sha256":    hex.EncodeToString(digest[:]), "synthetic_only": true,
		"model_calls": 0, "provider_tokenizer_used": false,
		"note":  "Verbatim evidence retention only; not model answer quality or customer success. Single paired timings include parse, selection/trimming and serialization.",
		"cases": results,
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRecallOfflineFallbackGuards(t *testing.T) {
	for _, protocol := range []string{"chat", "messages", "responses"} {
		for _, guard := range []string{"small", "busy", "cancelled", "deadline", "size", "items", "image", "broken_tool", "large_current", "large_tool", "external", "unknown"} {
			if (guard == "external" || guard == "unknown") && protocol != "responses" {
				continue
			}
			t.Run(protocol+"/"+guard, func(t *testing.T) {
				fixture := makeRecallFixture("old_named_file", protocol, 500000)
				p := fixture.Payload
				field := recallField(protocol)
				engine := newRecallExperiment()
				ctx := context.Background()
				want := ""
				switch guard {
				case "small":
					p = recallRender([]any{recallMessage("user", "small question")}, protocol)
					want = "not_needed"
				case "busy":
					engine.slots <- struct{}{}
					engine.slots <- struct{}{}
					want = "busy"
				case "cancelled":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
					want = "cancelled"
				case "deadline":
					engine.timeout = time.Nanosecond
					want = "deadline"
				case "size":
					engine.maxBytes = 100
					want = "size_limit"
				case "items":
					engine.maxItems = 2
					want = "item_limit"
				case "external":
					p["previous_response_id"] = "resp_synthetic_reference"
					want = "unsupported_state"
				case "unknown":
					p[field] = append(p[field].([]any), map[string]any{"type": "reasoning", "summary": []any{}})
					want = "unsupported_state"
				case "broken_tool":
					p[field] = p[field].([]any)[:len(p[field].([]any))-1]
					want = "unsupported_structure"
					if protocol == "responses" {
						want = "unsupported_state"
					}
				case "image":
					var block any = map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/synthetic.png"}}
					if protocol == "messages" {
						block = map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.invalid/synthetic.png"}}
					} else if protocol == "responses" {
						block = map[string]any{"type": "input_image", "image_url": "https://example.invalid/synthetic.png"}
					}
					p[field] = append(p[field].([]any), map[string]any{"role": "user", "content": []any{block}})
					want = "unsupported_structure"
				case "large_current":
					p = recallRender([]any{recallMessage("user", strings.Repeat("a", 400000)+"MIDDLE_EVIDENCE"+strings.Repeat("b", 400000))}, protocol)
					want = "mandatory_turn_too_large"
				case "large_tool":
					items := []any{recallMessage("user", "Read the entire response")}
					items = append(items, recallToolTurn("call_huge", "large.txt", strings.Repeat("a", 400000)+"MIDDLE_EVIDENCE"+strings.Repeat("b", 400000))...)
					p = recallRender(items, protocol)
					want = "mandatory_turn_too_large"
				}
				profile := recallProfile(p, protocol, 128000)
				raw := recallJSON(t, p)
				old, _, err := runRecallExperiment(context.Background(), raw, protocol, profile, nil)
				if err != nil {
					t.Fatal(err)
				}
				out, why, err := runRecallExperiment(ctx, raw, protocol, profile, engine)
				if err != nil {
					t.Fatal(err)
				}
				if why != want {
					t.Fatalf("reason=%s want=%s", why, want)
				}
				if !bytes.Equal(old, out) {
					t.Fatal("fallback changed baseline output")
				}
			})
		}
	}
	for _, raw := range [][]byte{[]byte("{"), []byte("[]"), []byte("null"), []byte("42")} {
		_, _, oldErr := runRecallExperiment(context.Background(), raw, "chat", contextTrimProfile{}, nil)
		_, _, newErr := runRecallExperiment(context.Background(), raw, "chat", contextTrimProfile{}, newRecallExperiment())
		if (oldErr == nil) != (newErr == nil) {
			t.Fatal("invalid JSON error behavior changed")
		}
	}
}

func TestRecallOfflinePerformance(t *testing.T) {
	arm := os.Getenv("RECALL_PERF_ARM")
	if arm == "" {
		t.Skip("set RECALL_PERF_ARM to baseline or candidate")
	}
	if arm != "baseline" && arm != "candidate" {
		t.Fatal("unknown arm")
	}
	f := makeRecallFixture("old_named_file", "chat", 1000000)
	raw := recallJSON(t, f.Payload)
	profile := recallProfile(f.Payload, "chat", 128000)
	var engine *recallExperiment
	if arm == "candidate" {
		engine = newRecallExperiment()
	}
	var measurements []recallMeasurement
	reasons := map[string]int{}
	for n := 0; n < 7; n++ {
		runtime.GC()
		_, reason, m := measuredRecall(t, raw, "chat", profile, engine)
		if n > 0 {
			measurements = append(measurements, m)
			reasons[reason]++
		}
	}
	var usage syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &usage)
	sort.Slice(measurements, func(i, j int) bool { return measurements[i].WallMS < measurements[j].WallMS })
	report := map[string]any{"arm": arm, "samples": measurements, "median_sample": measurements[len(measurements)/2], "peak_rss_mib": float64(usage.Maxrss) / 1024, "input_bytes": len(raw), "reasons": reasons}
	// Linux rusage high-water marks may include a pre-exec launcher. The
	// current address space's VmHWM/VmRSS are reported separately, never VmSize.
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(status), "\n") {
			parts := strings.Fields(line)
			if len(parts) == 3 && (parts[0] == "VmHWM:" || parts[0] == "VmRSS:") {
				value, _ := strconv.ParseFloat(parts[1], 64)
				report[strings.TrimSuffix(parts[0], ":")+"_mib"] = value / 1024
			}
		}
	}
	encoded := recallJSON(t, report)
	if path := os.Getenv("RECALL_PERF_REPORT"); path != "" {
		if err := os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Log(string(encoded))
}

func TestRecallOfflineConcurrencyAndDeterminism(t *testing.T) {
	f := makeRecallFixture("old_named_file", "chat", 1000000)
	raw := recallJSON(t, f.Payload)
	p := decodeRecall(t, raw)
	working, _ := cloneContextJSONValue(p["messages"]).([]any)
	baseline := selectContextItemsForBudget(working, 128000)
	engine := newContextRecallEngine()
	engine.timeout = 2 * time.Second // Race instrumentation is not a latency benchmark.
	const jobs = 6
	start := make(chan struct{})
	type result struct {
		output []any
		reason string
	}
	results := make(chan result, jobs)
	var wg sync.WaitGroup
	for i := 0; i < jobs; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			out, why := engine.choose(context.Background(), p, "chat", len(raw), 128000, nil, baseline)
			results <- result{out, why}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	selected, busy := 0, 0
	var reference []byte
	for r := range results {
		switch r.reason {
		case "busy":
			busy++
		case "selected":
			selected++
			data := recallJSON(t, r.output)
			if reference == nil {
				reference = data
			}
			if !bytes.Equal(reference, data) {
				t.Fatal("concurrent results are not deterministic")
			}
		default:
			t.Fatalf("unexpected concurrency result: %s", r.reason)
		}
	}
	if selected == 0 || busy == 0 {
		t.Fatalf("expected admitted and busy jobs; selected=%d busy=%d", selected, busy)
	}
	if len(engine.slots) != 0 {
		t.Fatal("recall slot leaked")
	}
	for i := 0; i < 3; i++ {
		out, why := engine.choose(context.Background(), p, "chat", len(raw), 128000, nil, baseline)
		if why != "selected" || !bytes.Equal(reference, recallJSON(t, out)) {
			t.Fatal("repeated selection changed")
		}
	}
	t.Logf("jobs=%d selected=%d busy=%d slots_released=true", jobs, selected, busy)
}
