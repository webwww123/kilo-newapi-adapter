package adapter

import (
	"encoding/json"
	"testing"
)

func TestSpaceBunnyDefaultOutputDoesNotOverrideExplicitBudgets(t *testing.T) {
	for _, path := range []string{"/chat/completions", "/messages", "/responses"} {
		for _, field := range []string{"", "max_tokens", "max_completion_tokens", "max_output_tokens"} {
			t.Run(path+"/"+field, func(t *testing.T) {
				payload := map[string]any{"model": spaceBunnyAlphaModel, "messages": []any{map[string]any{"role": "user", "content": "finish the report"}}, "input": "finish the report"}
				want := float64(spaceBunnyDefaultOutputTokens)
				if field != "" {
					payload[field] = float64(16000)
					want = 16000
				}
				body, _, err := normalizeTargetFreeModelRequest(mustJSON(t, payload), path)
				if err != nil {
					t.Fatal(err)
				}
				var got map[string]any
				if err := json.Unmarshal(body, &got); err != nil {
					t.Fatal(err)
				}
				canonical := "max_tokens"
				if path == "/responses" {
					canonical = "max_output_tokens"
				}
				if got[canonical] != want {
					t.Fatalf("effective output=%v want=%v", got[canonical], want)
				}
				_, again, err := normalizeTargetFreeModelRequest(body, path)
				if err != nil || again.OutputBudgetNormalized {
					t.Fatal("output budget changed on repeated normalization")
				}
			})
		}
	}
}

func TestSpaceBunnyDefaultOutputIsReservedByContextFitting(t *testing.T) {
	body, _, err := normalizeTargetFreeModelRequest([]byte(`{"model":"stealth/space-bunny-alpha","messages":[{"role":"user","content":"finish the report"}]}`), "/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	profile := contextTrimProfile{ContextLimit: 100000, SafetyMargin: 5000}
	var stats contextTrimStats
	budget := contextInputBudget(payload, profile, false, &stats)
	if budget != 100000-5000-contextTrimOverheadTokens-spaceBunnyDefaultOutputTokens {
		t.Fatalf("context budget %d did not reserve the emitted output allowance", budget)
	}
}
