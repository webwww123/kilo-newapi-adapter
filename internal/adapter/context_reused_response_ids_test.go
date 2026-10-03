package adapter

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

func reusedResponseCall() map[string]any {
	return map[string]any{"type": "function_call", "call_id": "reused", "name": "read", "arguments": `{"file":"notes"}`}
}

func reusedResponseResult(value string) map[string]any {
	return map[string]any{"type": "function_call_output", "call_id": "reused", "output": value}
}

func TestContextTrimKeepsCompletedReusedResponsesIDsInSeparateTurns(t *testing.T) {
	withContextTrimProfile(t, spaceBunnyAlphaModel, contextTrimProfile{ContextLimit: 6000})
	first, second := reusedResponseResult("earlier state"), reusedResponseResult("latest state")
	input := []any{
		map[string]any{"role": "system", "content": "Preserve the current task."},
		map[string]any{"role": "user", "content": strings.Repeat("old history ", 3000)},
		map[string]any{"role": "assistant", "content": "old answer"},
		map[string]any{"role": "user", "content": "Compare the two completed reads."},
		reusedResponseCall(), first, reusedResponseCall(), second,
	}
	body := mustJSON(t, map[string]any{"model": spaceBunnyAlphaModel, "max_output_tokens": 512, "input": input})
	out, stats, err := trimFreeModelContext(body, "/responses")
	if err != nil || stats.TrimmedItems == 0 {
		t.Fatalf("completed reused IDs blocked trimming: stats=%+v err=%v", stats, err)
	}
	kept := contextTrimItems(t, out, "input")
	if !contextResponsesItemsSafeToTrim(kept) || !contextToolHistorySafeToTrim(kept) {
		t.Fatal("trimmed repeated-ID turns are not complete")
	}
	groups := completedContextToolGroupsBefore(kept, len(kept))
	if len(groups) != 2 {
		t.Fatalf("kept %d tool groups, want both completed occurrences", len(groups))
	}
	if !reflect.DeepEqual(kept[len(kept)-4:], input[len(input)-4:]) {
		t.Fatal("call IDs, arguments, results, or chronological order changed")
	}
	if contextEstimateItemsTokens(kept) > 6000-512-contextTrimOverheadTokens {
		t.Fatal("trimmed request is still over budget")
	}
}

func TestContextTrimStillRejectsAmbiguousReusedResponsesIDs(t *testing.T) {
	for name, input := range map[string][]any{
		"overlapping calls":   {reusedResponseCall(), reusedResponseCall(), reusedResponseResult("a"), reusedResponseResult("b")},
		"duplicate result":    {reusedResponseCall(), reusedResponseResult("a"), reusedResponseResult("b")},
		"pending second call": {reusedResponseCall(), reusedResponseResult("a"), reusedResponseCall()},
		"result before call":  {reusedResponseResult("a"), reusedResponseCall()},
	} {
		t.Run(name, func(t *testing.T) {
			if contextResponsesItemsSafeToTrim(input) || contextToolHistorySafeToTrim(input) {
				t.Fatal("ambiguous tool history accepted")
			}
		})
	}
}

func TestContextTrimReusedResponsesIDsDoNotRelaxExternalStateGuard(t *testing.T) {
	withContextTrimProfile(t, spaceBunnyAlphaModel, contextTrimProfile{ContextLimit: 6000})
	input := []any{map[string]any{"role": "user", "content": strings.Repeat("history ", 4000)},
		reusedResponseCall(), reusedResponseResult("a"), reusedResponseCall(), reusedResponseResult("b")}
	for _, field := range []string{"previous_response_id", "conversation"} {
		body := mustJSON(t, map[string]any{"model": spaceBunnyAlphaModel, "max_output_tokens": 512, field: "external", "input": input})
		out, stats, err := trimFreeModelContext(body, "/responses")
		if err != nil || stats.changed() || !bytes.Equal(out, body) {
			t.Fatalf("external state guard changed: field=%s stats=%+v err=%v", field, stats, err)
		}
	}
}
