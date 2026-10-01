package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const stepCounterFakeWorker = `import json,sys,time
print(json.dumps({"ready":True,"revision":"5f6244077ac62e04eec3f320501ff8c2b293373a","clear_text_cache":True}),flush=True)
for line in sys.stdin:
 request=json.loads(line)
 if sys.argv[2]=="slow":time.sleep(5)
 if sys.argv[2]=="busy":time.sleep(.1)
 ident=request["id"]+(1 if sys.argv[2]=="mismatch" else 0)
 print(json.dumps({"id":ident,"tokens":int(sys.argv[1]),"compute_us":1}),flush=True)
`

func stepFakePool(t *testing.T, mode string, tokens int) *stepToolCounterPool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p, err := newStepToolCounterPool(ctx, []string{"python3", "-u", "-c", stepCounterFakeWorker, strconv.Itoa(tokens), mode})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestStepCounterDeadlineKillsExactOwnedProcess(t *testing.T) {
	p := stepFakePool(t, "slow", 40470)
	w := p.workers[0]
	pid := w.cmd.Process.Pid
	started := time.Now()
	_, err := p.Count(context.Background(), []any{map[string]any{"description": "fixture"}})
	if err == nil || err.Error() != "counter_deadline" || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("deadline fallback failed: %v", err)
	}
	if w.alive.Load() || syscall.Kill(pid, 0) != syscall.ESRCH {
		t.Fatal("timed-out native counter remained alive")
	}
	if len(p.idle) != 1 || len(p.cache) != 0 {
		t.Fatal("timed-out result entered cache or dead worker was reused")
	}
}

func TestStepCounterRejectsMismatchedReplyAndStopsWorker(t *testing.T) {
	p := stepFakePool(t, "mismatch", 40470)
	_, err := p.Count(context.Background(), []any{1})
	if err == nil || err.Error() != "counter_reply_guard" || len(p.idle) != 1 || len(p.cache) != 0 {
		t.Fatal("reply identity guard failed")
	}
}

func TestStepCounterBusyDoesNotQueue(t *testing.T) {
	p := stepFakePool(t, "busy", 100)
	done := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func(n int) { _, err := p.Count(context.Background(), []any{n}); done <- err }(i)
	}
	until := time.Now().Add(80 * time.Millisecond)
	for len(p.idle) != 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	_, err := p.Count(context.Background(), []any{3})
	if err == nil || err.Error() != "counter_busy" || time.Since(started) > 30*time.Millisecond {
		t.Fatal("busy request was queued")
	}
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestStepCounterCacheIsBoundedAndCanceledRequestsDoNotCount(t *testing.T) {
	p := stepFakePool(t, "normal", 100)
	for i := 0; i < 257; i++ {
		_, err := p.Count(context.Background(), []any{fmt.Sprint(i)})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(p.cache) != 256 {
		t.Fatal("cache exceeded bound")
	}
	result, err := p.Count(context.Background(), []any{"256"})
	if err != nil || !result.CacheHit {
		t.Fatal("count result was not reused")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Count(ctx, []any{"256"}); err == nil {
		t.Fatal("canceled request obtained count")
	}
	if _, err := p.Count(context.Background(), strings.Repeat("x", stepCounterSchemaMaxBytes)); err == nil {
		t.Fatal("oversized schema entered worker")
	}
}

func TestStepCounterNormalOtherModelAndMediaKeepOldBudget(t *testing.T) {
	p := stepFakePool(t, "normal", 40470)
	q := &contextRecallRequest{control: &contextRecallControl{stepTools: p}, stepToolsMode: "apply", stepToolsChosen: true, stepToolsPercent: 100, ctx: httptest.NewRequest("POST", "/v1/chat/completions", nil).Context(), event: contextRecallEvent{Protocol: "chat"}}
	tools := []any{map[string]any{"description": strings.Repeat("schema ", 23000)}}
	original := contextEstimateAnyTokens(tools)
	profile := contextTrimProfile{ContextLimit: 262144, MaxInputTokens: 220000}
	payload := map[string]any{"model": "stepfun/step-3.7-flash:free", "tools": tools, "messages": []any{map[string]any{"role": "user", "content": "short task"}}}
	if got := contextStepToolsTokens(payload, profile, 132096, q); got != original || p.served.Load() != 0 {
		t.Fatal("normal request invoked counter")
	}
	payload["messages"] = []any{map[string]any{"role": "user", "content": strings.Repeat("history ", 50000)}}
	payload["model"] = "other-model"
	if got := contextStepToolsTokens(payload, profile, 132096, q); got != original || p.served.Load() != 0 {
		t.Fatal("other model budget changed")
	}
	payload["model"] = "stepfun/step-3.7-flash:free"
	payload["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": strings.Repeat("a", 1000000)}}}}
	if got := contextStepToolsTokens(payload, profile, 132096, q); got != original || p.served.Load() != 0 {
		t.Fatal("media request budget changed")
	}
	payload["messages"] = []any{map[string]any{"role": "user", "content": strings.Repeat("history ", 50000)}}
	if got := contextStepToolsTokens(payload, profile, 132096); got != original || p.served.Load() != 0 {
		t.Fatal("missing request context invoked counter")
	}
	if got := contextStepToolsTokens(payload, profile, 132096, q); got != 48613 || p.served.Load() != 1 {
		t.Fatal("generic schema counter not used")
	}
}

func TestStepCounterHTTPDisconnectStopsWorkerAndKeepsBaseline(t *testing.T) {
	p := stepFakePool(t, "slow", 40470)
	payload := map[string]any{"model": "stepfun/step-3.7-flash:free", "max_completion_tokens": 128000,
		"tools": []any{map[string]any{"description": strings.Repeat("schema ", 23000)}},
		"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("old history ", 40000)},
			map[string]any{"role": "user", "content": "Keep my current task intact."}}}
	body, _ := json.Marshal(payload)
	baseline, _, err := trimFreeModelContext(body, "/chat/completions")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil).WithContext(ctx)
	control := &contextRecallControl{stepTools: p, engine: newContextRecallEngine(), emit: func(contextRecallEvent) {}}
	control.policy.Store(&contextRecallPolicy{Mode: "apply", Percent: 100, FragmentPercent: 100, StepToolsMode: "apply", StepToolsPercent: 100})
	control.nextReadNS.Store(time.Now().Add(time.Hour).UnixNano())
	q := control.begin(r)
	q.event.InputBytes = len(body)
	w := p.workers[0]
	pid := w.cmd.Process.Pid
	done := make(chan []byte, 1)
	go func() { out, _, _ := trimFreeModelContext(body, "/chat/completions", q); done <- out }()
	until := time.Now().Add(time.Second)
	for len(p.idle) == 2 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if len(p.idle) == 2 {
		t.Fatal("HTTP path never started the counter")
	}
	started := time.Now()
	cancel()
	select {
	case out := <-done:
		if string(out) != string(baseline) {
			t.Fatal("canceled count changed the old cropping result")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("HTTP cancellation did not stop preprocessing")
	}
	if time.Since(started) > 500*time.Millisecond || w.alive.Load() || syscall.Kill(pid, 0) != syscall.ESRCH || p.served.Load() != 0 || len(p.cache) != 0 {
		t.Fatal("disconnected request retained native work or accepted a result")
	}
	if q.ctx != r.Context() {
		t.Fatal("bounded preprocessing context escaped the trim")
	}
}

type stepCounterCustomJSON struct{ called *bool }

func (v stepCounterCustomJSON) MarshalJSON() ([]byte, error) {
	*v.called = true
	return []byte("null"), nil
}

func TestStepCounterGuardsBeforeSerialization(t *testing.T) {
	p := stepFakePool(t, "normal", 100)
	called := false
	for _, schema := range []any{
		stepCounterCustomJSON{&called},
		strings.Repeat("<", stepCounterSchemaMaxBytes/6+1),
		json.Number(strings.Repeat("9", stepCounterSchemaMaxBytes)),
	} {
		if _, err := p.Count(context.Background(), schema); err == nil {
			t.Fatal("unbounded schema was admitted")
		}
	}
	cycle := map[string]any{}
	cycle["child"] = cycle
	if _, err := p.Count(context.Background(), cycle); err == nil {
		t.Fatal("cyclic schema admitted")
	}
	if called || p.served.Load() != 0 || len(p.idle) != 2 {
		t.Fatal("guarded schema reached Marshal or a worker")
	}
	if _, err := p.Count(context.Background(), []any{map[string]any{"description": "ordinary 中文 schema <a>\n"}}); err != nil {
		t.Fatal("ordinary escaped JSON rejected")
	}
}

func TestStepCounterDeadWorkerRecoversWithoutRequestQueue(t *testing.T) {
	p := stepFakePool(t, "normal", 100)
	p.restartDelay = 10 * time.Millisecond // Before the first repair signal.
	old := p.workers[0]
	pid := old.cmd.Process.Pid
	if err := old.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-old.done
	started := time.Now()
	if _, err := p.Count(context.Background(), []any{"first"}); err == nil {
		t.Fatal("dead worker result accepted")
	}
	if time.Since(started) > 100*time.Millisecond {
		t.Fatal("request waited for worker replacement")
	}
	until := time.Now().Add(time.Second)
	for p.recovered.Load() != 1 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	p.mu.Lock()
	fresh := p.workers[0]
	slots := len(p.workers)
	p.mu.Unlock()
	if p.recovered.Load() != 1 || slots != 2 || fresh == old || fresh.cmd.Process.Pid == pid || !fresh.alive.Load() || syscall.Kill(pid, 0) != syscall.ESRCH {
		t.Fatal("owned dead process was not safely replaced")
	}
	// The first call borrows the original healthy slot, the next the new slot.
	for _, schema := range []string{"healthy-slot", "replacement-slot"} {
		if got, err := p.Count(context.Background(), []any{schema}); err != nil || got.Tokens != 100 {
			t.Fatal("pool did not serve after replacement")
		}
	}
}

func TestStepCounterCloseCancelsReplacementReadiness(t *testing.T) {
	state := filepath.Join(t.TempDir(), "owned-worker-starts")
	worker := `import json,os,sys,time
path=sys.argv[1]
with open(path,"a") as out:out.write(str(os.getpid())+"\n")
if len(open(path).read().splitlines())>2:time.sleep(5)
print(json.dumps({"ready":True,"revision":"5f6244077ac62e04eec3f320501ff8c2b293373a","clear_text_cache":True}),flush=True)
for line in sys.stdin:
 request=json.loads(line)
 print(json.dumps({"id":request["id"],"tokens":100}),flush=True)
`
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p, err := newStepToolCounterPool(ctx, []string{"python3", "-u", "-c", worker, state})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	p.restartDelay = 10 * time.Millisecond
	w := p.workers[0]
	_ = w.cmd.Process.Kill()
	<-w.done
	_, _ = p.Count(context.Background(), []any{"trigger-repair"})
	until := time.Now().Add(time.Second)
	var pids []string
	for time.Now().Before(until) {
		data, _ := os.ReadFile(state)
		pids = strings.Fields(string(data))
		if len(pids) == 3 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(pids) != 3 {
		t.Fatal("replacement readiness did not start")
	}
	started := time.Now()
	p.Close()
	if time.Since(started) > 500*time.Millisecond {
		t.Fatal("Close waited for replacement timeout")
	}
	for _, s := range pids {
		pid, _ := strconv.Atoi(s)
		if syscall.Kill(pid, 0) != syscall.ESRCH {
			t.Fatal("shutdown left an owned counter process")
		}
	}
}
