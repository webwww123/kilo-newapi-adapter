//go:build recall_offline

package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestContextRecallProductionSelection(t *testing.T) {
	for _, scenario := range recallScenarios {
		for _, protocol := range []string{"chat", "messages", "responses"} {
			f := makeRecallFixture(scenario, protocol, 1000000)
			raw := recallJSON(t, f.Payload)
			profile := recallProfile(f.Payload, protocol, 128000)
			old, _, err := runRecallExperiment(context.Background(), raw, protocol, profile, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{"shadow", "apply", "control", "busy"} {
				p := decodeRecall(t, raw)
				engine := newContextRecallEngine()
				q := &contextRecallRequest{control: &contextRecallControl{engine: engine}, ctx: context.Background(), chosen: mode != "control", event: contextRecallEvent{Mode: mode, Protocol: protocol, InputBytes: len(raw)}}
				if mode == "busy" {
					engine.slots <- struct{}{}
					engine.slots <- struct{}{}
					q.event.Mode = "apply"
				}
				var stats contextTrimStats
				if protocol == "responses" {
					trimResponsePayload(p, profile, &stats, q)
				} else {
					trimMessagePayload(p, "messages", profile, false, &stats, q)
				}
				out := recallJSON(t, p)
				validateRecallSelection(t, raw, out, protocol, 128000, q.event.Applied)
				if mode != "apply" || !q.event.Applied {
					if !bytes.Equal(out, old) {
						t.Fatalf("%s/%s/%s changed legacy fallback", scenario, protocol, mode)
					}
				} else if scenario == "old_named_file" || scenario == "early_chinese_constraint" || scenario == "historical_tool_result" || scenario == "continue_previous_task" || scenario == "corrected_decision" || scenario == "recent_control" {
					for _, fact := range f.Evidence {
						if !bytes.Contains(out, []byte(fact)) {
							t.Fatalf("lost required evidence: %s/%s", scenario, protocol)
						}
					}
				}
			}
		}
	}
}

func TestContextRecallHTTPIntegration(t *testing.T) {
	for _, protocol := range []string{"chat", "messages", "responses"} {
		path := map[string]string{"chat": "/v1/chat/completions", "messages": "/v1/messages", "responses": "/v1/responses"}[protocol]
		var forwarded []byte
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			forwarded, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"test_response":"unchanged"}`)
		}))
		f := makeRecallFixture("old_named_file", protocol, 1000000)
		// Use a model with the same three protocol forwarding path and no
		// special StepFun tool-choice or reasoning transformation.
		f.Payload["model"] = "kilo/nvidia/nemotron-3-super-120b-a12b:free"
		f.Payload["stream"] = false
		raw := recallJSON(t, f.Payload)
		controlPath := filepath.Join(t.TempDir(), "recall.json")
		var legacy []byte
		for _, mode := range []string{"off", "shadow", "apply"} {
			if err := os.WriteFile(controlPath, []byte(`{"mode":"`+mode+`","percent":100}`), 0600); err != nil {
				t.Fatal(err)
			}
			h := newBaseHandler(Config{UpstreamBaseURL: upstream.URL, ContextRecallConfigPath: controlPath, RequestLogDir: t.TempDir()})
			h.logger = nil
			// Race instrumentation changes CPU cost. This test checks the HTTP
			// contract; the separate size experiments measure production limits.
			h.contextRecall.engine.timeout = 5 * time.Second
			var events []contextRecallEvent
			h.contextRecall.emit = func(e contextRecallEvent) { events = append(events, e) }
			request := httptest.NewRequest("POST", path, bytes.NewReader(raw))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if response.Code != 200 || !strings.Contains(response.Body.String(), "unchanged") {
				t.Fatalf("response changed: %s/%s status=%d", protocol, mode, response.Code)
			}
			if mode == "off" {
				legacy = append([]byte(nil), forwarded...)
				continue
			}
			if len(events) != 1 {
				t.Fatalf("recall event missing %s/%s", protocol, mode)
			}
			if mode == "shadow" && !bytes.Equal(forwarded, legacy) {
				t.Fatal("shadow changed forwarded payload")
			}
			if mode == "apply" {
				if !events[0].Applied || !bytes.Contains(forwarded, []byte("orbit-47")) || bytes.Contains(legacy, []byte("orbit-47")) {
					t.Fatalf("real pipeline failed to recover old fact: %s", protocol)
				}
				var a, b map[string]any
				_ = json.Unmarshal(legacy, &a)
				_ = json.Unmarshal(forwarded, &b)
				delete(a, recallField(protocol))
				delete(b, recallField(protocol))
				if !reflect.DeepEqual(a, b) {
					t.Fatal("recall changed non-message fields")
				}
			}
		}
		upstream.Close()
	}
}
