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

// Additional task families fixed before changing the first selector. They
// include a deliberately implicit reference to an old tool result that the
// deployed trimmer already preserves. Recall must not conceal that regression.
func recallHoldouts() []recallFixture {
	types := []string{"holdout_named_fix", "holdout_implicit_tool", "holdout_joint_files", "holdout_recent_override", "holdout_adjacent_evidence", "holdout_continue", "holdout_translation", "holdout_total"}
	var fixtures []recallFixture
	pad := strings.Repeat("Unrelated operations archive: routine checks, office inventory, weather observations, scheduled maintenance. ", 120)
	for _, scenario := range types {
		items := []any{recallMessage("system", "Use the supplied history. Do not invent missing facts. SYSTEM_POLICY_731."), recallMessage("developer", "Return the requested final values. Later explicit corrections supersede earlier values. DEVELOPER_POLICY_619.")}
		question := "What is the exact fix flag recorded for src/queue/drain.rs? Return only the flag, or UNKNOWN."
		facts := []string{"--retain-offset=maple-628"}
		for n := 0; n < 80; n++ {
			text := fmt.Sprintf("Archive entry %03d.\n%s", n, pad)
			switch scenario {
			case "holdout_named_fix":
				if n == 7 {
					text = "src/queue/drain.rs exact fix flag: --retain-offset=maple-628.\n" + text
				}
			case "holdout_implicit_tool":
				question = "把之前工具读到的最终配置中的两个值原样报给我。只回答值；找不到就回答 UNKNOWN。"
				facts = []string{"indigo-462", "509"}
				if n == 22 {
					items = append(items, recallMessage("user", "Read the finalized deployment profile."))
					items = append(items, recallToolTurn("call_release_profile", "deploy/final_profile.json", "Final immutable delivery values: seal=indigo-462; lease=509.\n"+text)...)
					items = append(items, recallMessage("assistant", "Read completed."))
					continue
				}
			case "holdout_joint_files":
				question = "Report the exact flags for src/queue/drain.rs, src/queue/merge.rs, and src/queue/retry.rs, in that order. Use UNKNOWN for missing information."
				facts = []string{"cedar-117", "birch-283", "willow-694"}
				if n == 7 {
					text = "src/queue/drain.rs flag is cedar-117.\n" + text
				}
				if n == 31 {
					text = "src/queue/merge.rs flag is birch-283.\n" + text
				}
				if n == 49 {
					text = "src/queue/retry.rs flag is willow-694.\n" + text
				}
			case "holdout_recent_override":
				question = "Report the final confirmed export/manifest.rs signature, not a superseded proposal. Return only the signature or UNKNOWN."
				facts = []string{"coral-958"}
				if n == 5 {
					text = "export/manifest.rs proposed signature is coral-104, pending confirmation.\n" + text
				}
				if n == 78 {
					text = "Final correction for export/manifest.rs: use coral-958, revoke coral-104.\n" + text
				}
			case "holdout_adjacent_evidence":
				question = "What acceptance value was reported for service/ledger_finalize.py? Return only the value or UNKNOWN."
				facts = []string{"velvet-376"}
				if n == 19 {
					text = "We will discuss the acceptance value for service/ledger_finalize.py in the next entry.\n" + text
				}
				if n == 20 {
					text = "The value for that acceptance is velvet-376.\n" + text
				}
			case "holdout_continue":
				question = "继续刚才的任务，直接报需要保留的那个原值；找不到就回答 UNKNOWN。"
				facts = []string{"ochre-582"}
				if n == 11 {
					text = "service/watermark.py requires watermark ochre-582.\n" + text
				}
				if n == 79 {
					text = "下一步修复 service/watermark.py，请保留之前约定的 watermark。\n" + text
				}
			case "holdout_translation":
				question = "What settlement invariant and acceptance phrase must be preserved for this monetary reconciliation change? Return the phrase or UNKNOWN."
				facts = []string{"流水九六二"}
				if n == 4 {
					text = "资金凭证只准新增，严禁覆写，验收短语为流水九六二。\n" + text
				}
			case "holdout_total":
				question = "Calculate the sum of the delivery units across every archive entry in the complete conversation. Return the exact total, or INCOMPLETE if any entry is missing."
				facts = []string{"3240"}
				text = fmt.Sprintf("Delivery units in archive entry %03d: %d.\n", n, n+1) + text
			}
			items = append(items, recallMessage("user", text), recallMessage("assistant", "Recorded."))
		}
		items = append(items, recallMessage("user", question+" CURRENT_TASK_927. Answer from the conversation; do not call tools."))
		items = append(items, recallToolTurn("call_current", "workspace/current_state.txt", "CURRENT_TOOL_RESULT_518: ready.")...)
		fixtures = append(fixtures, recallFixture{Name: scenario, Protocol: "chat", Payload: recallRender(items, "chat"), Evidence: facts})
	}
	return fixtures
}

func TestRecallHoldoutExport(t *testing.T) {
	dir := os.Getenv("RECALL_HOLDOUT_EXPORT_DIR")
	if dir == "" {
		t.Skip("set RECALL_HOLDOUT_EXPORT_DIR")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, f := range recallHoldouts() {
		raw := recallJSON(t, f.Payload)
		profile := recallProfile(f.Payload, "chat", 64000)
		row := map[string]any{"scenario": f.Name, "expected": f.Evidence, "source_bytes": len(raw), "budget": 64000, "arms": map[string]any{}}
		for _, arm := range []string{"baseline", "candidate"} {
			var engine *recallExperiment
			if arm == "candidate" {
				engine = newRecallExperiment()
			}
			out, why, err := runRecallExperiment(context.Background(), raw, "chat", profile, engine)
			if err != nil {
				t.Fatal(err)
			}
			validateRecallSelection(t, raw, out, "chat", 64000, why == "selected")
			p := decodeRecall(t, out)
			hits := 0
			for _, fact := range f.Evidence {
				if strings.Contains(recallText(p["messages"]), fact) {
					hits++
				}
			}
			p["stream"] = true
			p["stream_options"] = map[string]any{"include_usage": true}
			p["max_tokens"] = 2048
			p["temperature"] = 0
			p["tool_choice"] = "none"
			out = recallJSON(t, p)
			name := f.Name + "-" + arm + ".json"
			if err := os.WriteFile(filepath.Join(dir, name), out, 0600); err != nil {
				t.Fatal(err)
			}
			row["arms"].(map[string]any)[arm] = map[string]any{"file": name, "bytes": len(out), "reason": why, "evidence_hits": hits}
			t.Logf("%s %s evidence=%d/%d reason=%s", f.Name, arm, hits, len(f.Evidence), why)
		}
		rows = append(rows, row)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), recallJSON(t, rows), 0600); err != nil {
		t.Fatal(err)
	}
}
