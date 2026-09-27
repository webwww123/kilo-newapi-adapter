//go:build recall_offline

package adapter

import (
	"bytes"
	"context"
	"testing"
)

func TestRecallConservativeGuards(t *testing.T) {
	for scenario, want := range map[string]string{
		"old_named_file":          "selected",
		"whole_history_aggregate": "broad_query",
		"semantic_paraphrase":     "no_recall_evidence",
		"keyword_distractors":     "no_recall_evidence",
	} {
		f := makeRecallFixture(scenario, "chat", 500000)
		raw := recallJSON(t, f.Payload)
		profile := recallProfile(f.Payload, "chat", 128000)
		engine := newRecallExperiment()
		engine.conservative = true
		out, why, err := runRecallExperiment(context.Background(), raw, "chat", profile, engine)
		if err != nil || why != want {
			t.Fatalf("%s: reason=%s want=%s err=%v", scenario, why, want, err)
		}
		if why != "selected" {
			old, _, err := runRecallExperiment(context.Background(), raw, "chat", profile, nil)
			if err != nil || !bytes.Equal(out, old) {
				t.Fatal("conservative rejection changed baseline output")
			}
		}
	}
	text := "Some 文件路径 repeated 文件路径 and src/queue/drain.rs checkpoint_checkpoint alpha alpha; alpha. unrelated beta gamma."
	all, n, ok := recallTerms(context.Background(), text)
	if !ok {
		t.Fatal("tokenization failed")
	}
	query := map[string]int{"文件": 1, "路径": 1, "src/queue/drain.rs": 1, "alpha": 1}
	filtered, count, ok := recallMatchingTerms(context.Background(), text, query)
	if !ok || count != n {
		t.Fatal("filtered index changed term-position count")
	}
	for term, occurrences := range all {
		if query[term] > 0 && filtered[term] != occurrences {
			t.Fatal("filtered index changed query term frequency")
		}
		if query[term] == 0 && filtered[term] != 0 {
			t.Fatal("index retained unrelated vocabulary")
		}
	}
}
