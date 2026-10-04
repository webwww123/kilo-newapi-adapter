package adapter

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func repeatedControlFixture() string {
	question := "What is FETCH_LIMIT?\n"
	head := question + strings.Repeat(" ", 2048-len(question))
	tail := "Latest state is CURRENT_REV_31.\n" + question
	tail = strings.Repeat(" ", 2048-len(tail)) + tail
	noise := strings.Repeat("FETCH_LIMIT pending review.\n", 20000)
	return head + noise[2048:270000] + "\nFETCH_LIMIT = middle-999\n" + noise[270000:] + tail
}

func TestContextRecallRepeatedRolloutAndTelemetry(t *testing.T) {
	original := repeatedControlFixture()
	baselineText := trimStringPreservingEdgesToContextBudget(original, 80000)
	if strings.Contains(baselineText, "middle-999") {
		t.Fatal("invalid middle-drop fixture")
	}
	for _, protocol := range []string{"chat/completions", "messages", "responses"} {
		for _, row := range []struct {
			mode    string
			percent int
			want    bool
		}{{"apply", 0, false}, {"apply", 100, true}, {"shadow", 100, false}} {
			path := filepath.Join(t.TempDir(), "policy.json")
			body, _ := json.Marshal(map[string]any{"mode": row.mode, "percent": 100, "fragment_percent": 100, "repeated_percent": row.percent})
			if err := os.WriteFile(path, body, 0600); err != nil {
				t.Fatal(err)
			}
			q := newContextRecallControl(path).begin(httptest.NewRequest("POST", "/v1/"+protocol, nil))
			field := "messages"
			if protocol == "responses" {
				field = "input"
			}
			item := func(text string) any { return map[string]any{"role": "user", "content": text} }
			payload := map[string]any{field: []any{item(original)}}
			baseline := []any{item(baselineText)}
			before, _ := json.Marshal(payload)
			out, applied := q.selectTextItems(payload, field, baseline)
			if applied != row.want {
				t.Fatal(protocol, row, q.event.Reason)
			}
			if q.event.RepeatedEnabled != (row.percent == 100) || q.event.RepeatedApplied != row.want {
				t.Fatal("new fallback rollout metadata does not describe selection", q.event)
			}
			if row.percent == 0 && (q.event.RepeatedAttempted || q.event.RepeatedMatched || q.event.Reason != "fragment_no_evidence") {
				t.Fatal("disabled fallback ran or changed legacy outcome", q.event)
			}
			if row.percent == 100 && (!q.event.RepeatedAttempted || !q.event.RepeatedMatched) {
				t.Fatal("actual repeated-query attempt is not observable", q.event)
			}
			if row.want {
				text := out[0].(map[string]any)["content"].(string)
				source := []rune(original)
				if !strings.Contains(text, "FETCH_LIMIT = middle-999") || !strings.Contains(text, "CURRENT_REV_31") || !strings.HasSuffix(text, string(source[contextRecallQuestionStart(source):])) || newContextRecallEngine().fragmentCost(text) > newContextRecallEngine().fragmentCost(baselineText) {
					t.Fatal("fallback missed evidence, recent state, or capacity protection")
				}
			}
			after, _ := json.Marshal(payload)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("mutated source input")
			}
		}
	}
}

func TestContextRecallRepeatedHotDisablePreservesExistingPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	c := newContextRecallControl(path)
	for _, pct := range []int{100, 0, 101, -1, 25} {
		body, _ := json.Marshal(map[string]any{"mode": "apply", "percent": 100, "fragment_percent": 100, "repeated_percent": pct})
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		c.nextReadNS.Store(0)
		policy := c.current()
		want := pct
		if pct < 0 || pct > 100 {
			want = 0
		}
		if policy.Percent != 100 || policy.FragmentPercent != 100 || policy.RepeatedPercent != want {
			t.Fatal("new flag invalidated existing recall or failed hot reload", policy)
		}
	}
	if contextRepeatedEnabled(context.Background()) {
		t.Fatal("missing rollout context enabled fallback")
	}
}
