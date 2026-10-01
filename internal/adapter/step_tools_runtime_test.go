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

func stepRuntimePayload() map[string]any {
	return map[string]any{"model": "stepfun/step-3.7-flash:free", "max_completion_tokens": 128000,
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "Read", "description": strings.Repeat("schema ", 23000), "parameters": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}}}}},
		// This complete older unit fits the counted budget but not the old one.
		"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("old history ", 12000)}, map[string]any{"role": "user", "content": "Keep my current task intact."}}}
}

func stepRuntimeTrim(t *testing.T, payload map[string]any, pool *stepToolCounterPool, mode string, percent int) ([]byte, *contextRecallRequest) {
	t.Helper()
	body, _ := json.Marshal(payload)
	control := &contextRecallControl{stepTools: pool, engine: newContextRecallEngine(), emit: func(contextRecallEvent) {}}
	control.policy.Store(&contextRecallPolicy{Mode: "apply", Percent: 0, FragmentPercent: 0, StepToolsMode: mode, StepToolsPercent: percent})
	control.nextReadNS.Store(time.Now().Add(time.Hour).UnixNano())
	q := control.begin(httptest.NewRequest("POST", "/v1/chat/completions", nil))
	if q != nil {
		q.event.InputBytes = len(body)
	}
	out, _, err := trimFreeModelContext(body, "/chat/completions", q)
	if err != nil {
		t.Fatal(err)
	}
	return out, q
}

func TestStepToolsShadowAndDisabledKeepExactLegacyInput(t *testing.T) {
	p := stepFakePool(t, "normal", 40470)
	payload := stepRuntimePayload()
	legacy, _ := stepRuntimeTrim(t, payload, nil, "", 0)
	off, q := stepRuntimeTrim(t, payload, p, "", 0)
	if !reflect.DeepEqual(off, legacy) || q != nil || p.served.Load() != 0 {
		t.Fatal("disabled policy changed legacy behavior")
	}
	shadow, q := stepRuntimeTrim(t, payload, p, "shadow", 100)
	if !reflect.DeepEqual(shadow, legacy) || q.event.ToolsApplied || !q.event.ToolsAttempted || q.event.ToolsReason != "shadow_counted" || q.event.ToolsBudgetAfter <= q.event.ToolsBudgetBefore {
		t.Fatal("shadow changed the model input or failed to report candidate count")
	}
	apply, q := stepRuntimeTrim(t, payload, p, "apply", 100)
	if reflect.DeepEqual(apply, legacy) || !q.event.ToolsApplied || !q.event.ToolsCacheHit || q.event.ToolsReason != "budget_applied" {
		t.Fatalf("apply: budget %d->%d, applied=%t cache=%t reason=%s unchanged=%t", q.event.ToolsBudgetBefore, q.event.ToolsBudgetAfter, q.event.ToolsApplied, q.event.ToolsCacheHit, q.event.ToolsReason, reflect.DeepEqual(apply, legacy))
	}
	var got map[string]any
	if json.Unmarshal(apply, &got) != nil || !reflect.DeepEqual(got["tools"], payload["tools"]) || got["max_completion_tokens"] != float64(128000) {
		t.Fatal("fixed request fields changed")
	}
	unsampled, q := stepRuntimeTrim(t, payload, p, "apply", 0)
	if !reflect.DeepEqual(unsampled, legacy) || q.event.ToolsApplied || q.event.ToolsReason != "not_sampled" {
		t.Fatal("control group changed input")
	}
}

func TestStepToolsInvalidHotFlagPreservesExistingRecall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.json")
	if os.WriteFile(path, []byte(`{"mode":"apply","percent":100,"fragment_percent":100,"repeated_percent":100,"step_tools_mode":"bad","step_tools_percent":100}`), 0600) != nil {
		t.Fatal("cannot write fixture policy")
	}
	p := newContextRecallControl(path).current()
	if p.Percent != 100 || p.FragmentPercent != 100 || p.RepeatedPercent != 100 || p.StepToolsMode != "" || p.StepToolsPercent != 0 {
		t.Fatal("invalid new flag disabled existing recall")
	}
	if os.WriteFile(path, []byte(`{"mode":"apply","percent":100,"fragment_percent":100,"step_tools_mode":"apply","step_tools_percent":101}`), 0600) != nil {
		t.Fatal("cannot write fixture policy")
	}
	p = newContextRecallControl(path).current()
	if p.Percent != 100 || p.StepToolsPercent != 0 {
		t.Fatal("invalid sample rate applied")
	}
}

func TestStepToolsUnavailableWorkerAndOtherModelsKeepLegacyInput(t *testing.T) {
	payload := stepRuntimePayload()
	legacy, _ := stepRuntimeTrim(t, payload, nil, "", 0)
	out, q := stepRuntimeTrim(t, payload, nil, "apply", 100)
	if !reflect.DeepEqual(out, legacy) || q.event.ToolsReason != "worker_unavailable" {
		t.Fatal("missing dependency affected input")
	}
	p := stepFakePool(t, "normal", 40470)
	payload["model"] = "other-model"
	legacy, _ = stepRuntimeTrim(t, payload, nil, "", 0)
	out, q = stepRuntimeTrim(t, payload, p, "apply", 100)
	if !reflect.DeepEqual(out, legacy) || q.event.ToolsEligible || p.served.Load() != 0 {
		t.Fatal("unrelated model counted or changed")
	}
}

func TestStepToolsRealBundle(t *testing.T) {
	bundle := os.Getenv("STEP_TOOLS_TEST_BUNDLE")
	if bundle == "" {
		t.Skip("formal CI runtime bundle not supplied")
	}
	control := &contextRecallControl{}
	control.startStepTools(bundle)
	if control.stepTools == nil {
		t.Fatal("formal runtime did not start")
	}
	t.Cleanup(control.closeStepTools)
	pool := control.stepTools
	result, err := pool.Count(context.Background(), stepRuntimePayload()["tools"])
	if err != nil || result.Tokens <= 0 || result.CacheHit {
		t.Fatal("native runtime did not count the schema")
	}
	payload := stepRuntimePayload()
	legacy, _ := stepRuntimeTrim(t, payload, nil, "", 0)
	shadow, q := stepRuntimeTrim(t, payload, pool, "shadow", 100)
	if !reflect.DeepEqual(shadow, legacy) || q.event.ToolsReason != "shadow_counted" {
		t.Fatal("native shadow changed input")
	}
	out, q := stepRuntimeTrim(t, payload, pool, "apply", 100)
	if !q.event.ToolsApplied || reflect.DeepEqual(out, legacy) {
		t.Fatal("native runtime did not release input budget")
	}
	pool.Close()
	for _, worker := range pool.workers {
		if worker.alive.Load() {
			t.Fatal("native worker remained alive")
		}
	}
}

func TestStepToolsSidecarMetadataOnlyDoesNotWriteRequestBodies(t *testing.T) {
	dir := t.TempDir()
	h := newBaseHandler(Config{ContextRecallMetadataOnly: true, RequestLogEnabled: true, RequestLogDir: dir})
	if h.logger != nil {
		t.Fatal("metadata sidecar enabled another body logger")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("metadata sidecar created body log files")
	}
}

func TestStepToolsEnvironmentSelectsMetadataOnly(t *testing.T) {
	t.Setenv("CONTEXT_RECALL_METADATA_ONLY", "true")
	t.Setenv("CONTEXT_STEP_TOOLS_BUNDLE", " /trusted/public/runtime ")
	cfg := ConfigFromEnv()
	if !cfg.ContextRecallMetadataOnly || cfg.StepToolsBundlePath != "/trusted/public/runtime" {
		t.Fatal("sidecar environment flags were ignored")
	}
	t.Setenv("CONTEXT_RECALL_METADATA_ONLY", "false")
	if ConfigFromEnv().ContextRecallMetadataOnly {
		t.Fatal("ordinary service logging default changed")
	}
}
