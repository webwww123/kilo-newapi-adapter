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
	"time"
)

func fragmentFixture(needle string) string {
	filler := "alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee zulu. "
	left := strings.Repeat(filler, 1500)
	return "Read the document and return the value of ARCHIVE_TARGET and its two neighboring lines.\n<<<START>>>\n" + left + "\nNEIGHBOR_BEFORE_7f3a\n" + needle + "\nNEIGHBOR_AFTER_7f3a\n" + left + "\n<<<END>>>\nCurrent state is REVISION_20260929; keep it. What is ARCHIVE_TARGET?"
}

func TestContextRecallFragmentsRecoverWithoutTouchingTail(t *testing.T) {
	engine := newContextRecallEngine()
	for _, needle := range []string{"ARCHIVE_TARGET = 92af6e1b", "ARCHIVE_TARGET(path=src/payments/service.go): do not modify invoice_id; use integer cents", "ARCHIVE_TARGET：北门口令是青石；人物约定不得伤害守卫"} {
		original := fragmentFixture(needle)
		baseline := trimStringPreservingEdgesToContextBudget(original, 80000)
		if strings.Contains(baseline, needle) {
			t.Fatal("fixture must reproduce middle loss")
		}
		out, reason := engine.recallText(context.Background(), original, baseline)
		if reason != "fragment_selected" || !strings.Contains(out, "\nNEIGHBOR_BEFORE_7f3a\n"+needle+"\nNEIGHBOR_AFTER_7f3a\n") {
			t.Fatalf("did not recover source neighborhood: %s", reason)
		}
		old := []rune(baseline)
		if !strings.HasPrefix(out, string([]rune(original)[:8192])) || !strings.HasSuffix(out, string(old[len(old)/4:])) {
			t.Fatal("protected edges changed")
		}
		if engine.fragmentCost(out) > engine.fragmentCost(baseline) {
			t.Fatal("grew input budget")
		}
		if !strings.Contains(out, "REVISION_20260929") {
			t.Fatal("lost latest state")
		}
	}
}

func TestContextRecallFragmentsNegativeAndFailOpen(t *testing.T) {
	r := newContextRecallEngine()
	original := fragmentFixture("no matching evidence")
	baseline := trimStringPreservingEdgesToContextBudget(original, 80000)
	if _, why := r.recallText(context.Background(), original, baseline); why != "fragment_no_evidence" {
		t.Fatalf("invented absent evidence: %s", why)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, why := r.recallText(ctx, original, baseline); why != "fragment_deadline" {
		t.Fatal(why)
	}
	r.slots <- struct{}{}
	r.slots <- struct{}{}
	if _, why := r.recallText(context.Background(), original, baseline); why != "fragment_busy" {
		t.Fatal(why)
	}
	<-r.slots
	<-r.slots
	if _, why := r.recallText(context.Background(), original, original); why != "fragment_not_needed" {
		t.Fatal(why)
	}
}

func TestContextRecallFragmentsCustomerReport(t *testing.T) {
	unit := "alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee zulu. "
	filler := strings.Repeat(unit, 423588/len(unit)+2)
	task := "Audit the document below for three exact markers and the two neighbor tokens that sit on the lines immediately before and after the middle marker. Reply with JSON only, no markdown: {\"start\":\"PRESENT\",\"mid\":\"PRESENT\",\"end\":\"PRESENT\",\"neighbor_before\":\"PRESENT\",\"neighbor_after\":\"PRESENT\"} using PRESENT or MISSING for each. Do not restate the document. Markers: <<<START>>> <<<MIDDLE>>> <<<END>>>. Neighbors: NEIGHBOR_BEFORE_7f3a NEIGHBOR_AFTER_7f3a.\n\n"
	needle := "\nNEIGHBOR_BEFORE_7f3a\n<<<MIDDLE>>>\nNEIGHBOR_AFTER_7f3a\n"
	original := task + "<<<START>>>\n" + filler[:211794] + needle + filler[:211794] + "\n<<<END>>>\nEnd of document. Reply with the JSON verdict now.\n"
	if len(original) != 424168 {
		t.Fatal("customer fixture changed")
	}
	baseline := trimStringPreservingEdgesToContextBudget(original, 60000)
	if strings.Contains(baseline, needle) {
		t.Fatal("fixture no longer exercises discarded middle")
	}
	out, why := newContextRecallEngine().recallText(context.Background(), original, baseline)
	if why != "fragment_selected" || !strings.Contains(out, needle) {
		t.Fatal("customer document neighborhood absent", why)
	}
}

func TestContextRecallFragmentsProtocolTextBlocks(t *testing.T) {
	text := fragmentFixture("ARCHIVE_TARGET = protocol-value")
	for _, protocol := range []string{"chat/completions", "messages", "responses"} {
		path := filepath.Join(t.TempDir(), "policy.json")
		os.WriteFile(path, []byte(`{"mode":"apply","percent":100,"fragment_percent":100}`), 0600)
		q := newContextRecallControl(path).begin(httptest.NewRequest("POST", "/v1/"+protocol, nil))
		field, blockType := "messages", "text"
		if protocol == "responses" {
			field, blockType = "input", "input_text"
		}
		content := func(v string) []any { return []any{map[string]any{"type": blockType, "text": v}} }
		payload := map[string]any{field: []any{map[string]any{"role": "user", "content": content(text)}}}
		baseline := []any{map[string]any{"role": "user", "content": content(trimStringPreservingEdgesToContextBudget(text, 80000))}}
		out, applied := q.selectTextItems(payload, field, baseline)
		if !applied {
			t.Fatal(protocol, q.event.Reason)
		}
		parts := out[0].(map[string]any)["content"].([]any)
		if len(parts) != 1 || parts[0].(map[string]any)["type"] != blockType {
			t.Fatal("protocol structure changed")
		}
	}
}

func TestContextRecallFragmentsControlAndStructure(t *testing.T) {
	original := fragmentFixture("ARCHIVE_TARGET = 91273")
	baselineText := trimStringPreservingEdgesToContextBudget(original, 80000)
	system := map[string]any{"role": "system", "content": "Never modify the current instruction."}
	toolCall := map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "t1", "type": "function", "function": map[string]any{"name": "edit", "arguments": "{}"}}}}
	toolResult := map[string]any{"role": "tool", "tool_call_id": "t1", "content": "latest tool output untouched"}
	payload := map[string]any{"messages": []any{system, map[string]any{"role": "user", "content": original}, toolCall, toolResult}}
	baseline := []any{system, map[string]any{"role": "user", "content": baselineText}, toolCall, toolResult}
	encoded, _ := json.Marshal(payload)
	for _, pct := range []int{0, 100} {
		path := filepath.Join(t.TempDir(), "policy.json")
		b, _ := json.Marshal(map[string]any{"mode": "apply", "percent": 100, "fragment_percent": pct})
		os.WriteFile(path, b, 0600)
		control := newContextRecallControl(path)
		request := control.begin(httptest.NewRequest("POST", "/v1/chat/completions", nil))
		out, applied := request.selectTextItems(payload, "messages", baseline)
		if applied != (pct == 100) {
			t.Fatal("fragment rollout not independent", request.event.Reason)
		}
		if pct == 100 && (!reflect.DeepEqual(out[0], system) || !reflect.DeepEqual(out[2:], baseline[2:])) {
			t.Fatal("modified tools or system")
		}
		if pct == 0 && request.event.Reason != "fragment_control" {
			t.Fatal("silent unobserved legacy truncation")
		}
	}
	after, _ := json.Marshal(payload)
	if string(encoded) != string(after) {
		t.Fatal("mutated original payload")
	}
}

func BenchmarkContextRecallFragments(b *testing.B) {
	r := newContextRecallEngine()
	original := fragmentFixture("ARCHIVE_TARGET = benchmark-value")
	baseline := trimStringPreservingEdgesToContextBudget(original, 80000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		_, why := r.recallText(context.Background(), original, baseline)
		if why != "fragment_selected" {
			b.Fatal(why)
		}
		if time.Since(start) > time.Second {
			b.Fatal("unbounded selection")
		}
	}
}
