//go:build recall_offline

package adapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Export synthetic A/B requests only. This test does not contact any service.
// The questions identify the information needed, without disclosing its value.
func TestRecallAnswerProbeExport(t *testing.T) {
	dir := os.Getenv("RECALL_ANSWER_EXPORT_DIR")
	if dir == "" {
		t.Skip("set RECALL_ANSWER_EXPORT_DIR to export synthetic probes")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	questions := map[string]string{
		"old_named_file":           "根据之前记录，db/replay_worker.go 的 checkpoint key 是什么？只回答 key；找不到就回答 UNKNOWN。",
		"early_chinese_constraint": "最早对退款任务的幂等校验有什么约定，验收码是什么？只回答约定和验收码；找不到就回答 UNKNOWN。",
		"historical_tool_result":   "根据之前读取的 shard/lease_clock.go 结果，lease TTL 是多少秒，测量 marker 是什么？只回答数值和 marker；找不到就回答 UNKNOWN。",
		"continue_previous_task":   "继续。请直接回答上一项修复要求保留的 checkpoint key；找不到就回答 UNKNOWN。",
		"corrected_decision":       "gateway/session.go 最后确认的连接超时是多少毫秒？只回答最终数值；找不到就回答 UNKNOWN。",
		"semantic_paraphrase":      "For financial integrity in the reconciliation service, what was the earlier immutable-record requirement and its acceptance phrase? If unavailable answer UNKNOWN.",
		"keyword_distractors":      "For mesh_probe.go socket timeout and retry policy, after which state checkpoint is retry forbidden? Answer only the checkpoint or UNKNOWN.",
		"whole_history_aggregate":  "List the clearance values from every observatory record, in chronological order. Include all available values; do not invent missing values.",
		"recent_control":           "sensor_recent.go 最新报告的 calibration 数值是什么？只回答该数值；找不到就回答 UNKNOWN。",
	}
	want := map[string][]string{
		"old_named_file": {"orbit-47"}, "early_chinese_constraint": {"refund-keep-812"},
		"historical_tool_result": {"413", "nickel-72"}, "continue_previous_task": {"luna-83"},
		"corrected_decision": {"1700"}, "semantic_paraphrase": {"账本八三七"},
		"keyword_distractors": {"cedar-29"}, "recent_control": {"cyan-615"},
	}
	manifest := []map[string]any{}
	for _, scenario := range recallScenarios {
		fixture := makeRecallFixture(scenario, "chat", 1000000)
		items := fixture.Payload["messages"].([]any)
		for i := len(items) - 1; i >= 0; i-- {
			if recallOrdinaryUser(items[i]) {
				items[i].(map[string]any)["content"] = questions[scenario] + " CURRENT_TASK_927. Use the existing conversation only; do not call tools."
				break
			}
		}
		profile := recallProfile(fixture.Payload, "chat", 128000)
		raw := recallJSON(t, fixture.Payload)
		row := map[string]any{"scenario": scenario, "expected": want[scenario], "source_bytes": len(raw), "budget": 128000, "arms": map[string]any{}}
		for _, arm := range []string{"baseline", "candidate"} {
			var candidate *recallExperiment
			if arm == "candidate" {
				candidate = newRecallExperiment()
			}
			out, reason, err := runRecallExperiment(context.Background(), raw, "chat", profile, candidate)
			if err != nil {
				t.Fatal(err)
			}
			validateRecallSelection(t, raw, out, "chat", 128000, reason == "selected")
			p := decodeRecall(t, out)
			p["stream"] = true
			p["stream_options"] = map[string]any{"include_usage": true}
			p["max_tokens"] = 2048
			p["temperature"] = 0
			p["tool_choice"] = "none"
			out = recallJSON(t, p)
			name := scenario + "-" + arm + ".json"
			if err := os.WriteFile(filepath.Join(dir, name), out, 0600); err != nil {
				t.Fatal(err)
			}
			row["arms"].(map[string]any)[arm] = map[string]any{"file": name, "bytes": len(out), "reason": reason}
		}
		manifest = append(manifest, row)
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0600); err != nil {
		t.Fatal(err)
	}
}
