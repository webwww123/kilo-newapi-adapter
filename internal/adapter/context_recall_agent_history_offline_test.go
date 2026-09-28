//go:build recall_offline

package adapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Agent clients commonly have a small number of user tasks and many tool
// rounds. The earlier 80/96-user-turn fixtures do not represent that shape.
func recallAgentHistory(protocol string) recallFixture {
	items := []any{recallMessage("system", "Use the original tool observations. SYSTEM_POLICY_731."), recallMessage("developer", "Preserve exact values. DEVELOPER_POLICY_619."), recallMessage("user", "Inspect the repository and record configuration findings.")}
	pad := strings.Repeat("Unrelated module output: routine sampling, geometry metadata, build timestamps, asset inventory. ", 210)
	for n := 0; n < 80; n++ {
		path := fmt.Sprintf("archive/component_%03d.txt", n)
		output := fmt.Sprintf("Inspection %03d.\n%s", n, pad)
		if n == 8 {
			path = "src/queue/flush_policy.rs"
			output = "FINAL_BATCH_KEY = amber-931;\n" + output
		}
		items = append(items, recallToolTurn(fmt.Sprintf("call_scan_%03d", n), path, output)...)
		items = append(items, recallMessage("assistant", "Inspection recorded; continuing the same task."))
	}
	items = append(items, recallMessage("user", "According to the earlier read of src/queue/flush_policy.rs, what is FINAL_BATCH_KEY? Return only its value, or UNKNOWN. CURRENT_TASK_927."))
	items = append(items, recallToolTurn("call_current", "workspace/current_state.txt", "CURRENT_TOOL_RESULT_518: ready.")...)
	return recallFixture{Name: "few_user_many_tools", Protocol: protocol, Payload: recallRender(items, protocol), Evidence: []string{"amber-931"}}
}

func TestRecallAgentHistoryAudit(t *testing.T) {
	var rows []map[string]any
	for _, protocol := range []string{"chat", "messages", "responses"} {
		f := recallAgentHistory(protocol)
		raw := recallJSON(t, f.Payload)
		profile := recallProfile(f.Payload, protocol, 128000)
		old, _, err := runRecallExperiment(context.Background(), raw, protocol, profile, nil)
		if err != nil {
			t.Fatal(err)
		}
		p := decodeRecall(t, raw)
		baseline := decodeRecall(t, old)[recallField(protocol)].([]any)
		engine := newContextRecallEngine()
		kept, why := engine.choose(context.Background(), p, protocol, len(raw), 128000, baseline)
		candidate := old
		if why == "selected" {
			p[recallField(protocol)] = kept
			candidate = recallJSON(t, p)
		}
		validateRecallSelection(t, raw, candidate, protocol, 128000, why == "selected")
		row := map[string]any{"protocol": protocol, "bytes": len(raw), "reason": why, "baseline_hit": strings.Contains(string(old), "amber-931"), "candidate_hit": strings.Contains(string(candidate), "amber-931")}
		rows = append(rows, row)
		t.Logf("%s", recallJSON(t, row))
	}
	if path := os.Getenv("RECALL_AGENT_REPORT"); path != "" {
		if err := os.WriteFile(path, recallJSON(t, rows), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestContextRecallAgentToolPaths(t *testing.T) {
	for _, protocol := range []string{"chat", "messages", "responses"} {
		f := recallAgentHistory(protocol)
		items := f.Payload[recallField(protocol)].([]any)
		for i := len(items) - 1; i >= 0; i-- {
			if contextRecallOrdinaryUser(items[i]) {
				items[i].(map[string]any)["content"] = "What value was configured in the earlier read of src/queue/flush_policy.rs? Return only the value, or UNKNOWN. CURRENT_TASK_927."
				break
			}
		}
		raw := recallJSON(t, f.Payload)
		working, _ := cloneContextJSONValue(items).([]any)
		baseline := selectContextItemsForBudget(working, 128000)
		kept, why := newContextRecallEngine().choose(context.Background(), f.Payload, protocol, len(raw), 128000, baseline)
		if why != "selected" {
			t.Fatalf("tool path did not trigger recall: %s %s", protocol, why)
		}
		f.Payload[recallField(protocol)] = kept
		out := recallJSON(t, f.Payload)
		validateRecallSelection(t, raw, out, protocol, 128000, true)
		if !strings.Contains(string(out), "amber-931") {
			t.Fatalf("tool argument path was not searchable: %s", protocol)
		}
	}
}

func TestRecallAgentAnswerExport(t *testing.T) {
	dir := os.Getenv("RECALL_AGENT_EXPORT_DIR")
	if dir == "" {
		t.Skip("set RECALL_AGENT_EXPORT_DIR")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	f := recallAgentHistory("chat")
	items := f.Payload["messages"].([]any)
	for i := len(items) - 1; i >= 0; i-- {
		if contextRecallOrdinaryUser(items[i]) {
			items[i].(map[string]any)["content"] = "What value was configured in the earlier read of src/queue/flush_policy.rs? Return only the value, or UNKNOWN. CURRENT_TASK_927. Do not call tools."
			break
		}
	}
	raw := recallJSON(t, f.Payload)
	profile := recallProfile(f.Payload, "chat", 128000)
	row := map[string]any{"scenario": f.Name, "expected": f.Evidence, "budget": 128000, "source_bytes": len(raw), "arms": map[string]any{}}
	for _, arm := range []string{"baseline", "candidate"} {
		out, why, err := runRecallExperiment(context.Background(), raw, "chat", profile, nil)
		if err != nil {
			t.Fatal(err)
		}
		if arm == "candidate" {
			baseline := decodeRecall(t, out)["messages"].([]any)
			kept, reason := newContextRecallEngine().choose(context.Background(), f.Payload, "chat", len(raw), 128000, baseline)
			why = reason
			if why != "selected" {
				t.Fatalf("agent selection failed: %s", why)
			}
			p := decodeRecall(t, raw)
			p["messages"] = kept
			out = recallJSON(t, p)
		}
		validateRecallSelection(t, raw, out, "chat", 128000, arm == "candidate")
		p := decodeRecall(t, out)
		p["max_tokens"] = 2048
		p["stream_options"] = map[string]any{"include_usage": true}
		p["tool_choice"] = "none"
		p["temperature"] = 0
		out = recallJSON(t, p)
		name := f.Name + "-" + arm + ".json"
		if err := os.WriteFile(filepath.Join(dir, name), out, 0600); err != nil {
			t.Fatal(err)
		}
		row["arms"].(map[string]any)[arm] = map[string]any{"file": name, "bytes": len(out), "reason": why}
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), recallJSON(t, []any{row}), 0600); err != nil {
		t.Fatal(err)
	}
}
