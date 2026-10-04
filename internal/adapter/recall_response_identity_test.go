package adapter

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRecallResponseIdentityWrittenBytesAndFailure(t *testing.T) {
	q := &contextRecallRequest{ctx: context.Background(), event: contextRecallEvent{ToolsEligible: true}}
	q.observeResponseWrite([]byte("abc"), 3, nil)
	q.observeResponseWrite([]byte("def"), 1, io.ErrShortWrite)
	q.finishResponseIdentity()
	sum := sha256.Sum256([]byte("abcd"))
	if q.event.ResponseBodySHA256 != hex.EncodeToString(sum[:]) || q.event.ResponseBodyBytes != 4 || !q.event.ResponseWriteFailed {
		t.Fatal("partial write did not hash only accepted bytes and record failure")
	}
}

func TestRecallResponseIdentityLimitCancellationAndScope(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	q := &contextRecallRequest{ctx: ctx, event: contextRecallEvent{ToolsEligible: true}}
	chunk := make([]byte, 64<<10)
	for i := 0; i <= recallResponseIdentityLimit/len(chunk); i++ {
		q.observeResponseWrite(chunk, len(chunk), nil)
	}
	cancel()
	q.finishResponseIdentity()
	if !q.event.ResponseHashLimited || q.event.ResponseBodySHA256 != "" || !q.event.ResponseRequestCanceled {
		t.Fatal("bounded/canceled digest must not qualify")
	}
	unqualified := &contextRecallRequest{}
	unqualified.observeResponseWrite([]byte("unqualified"), 11, nil)
	if unqualified.responseIdentity != nil {
		t.Fatal("unqualified request retained observer state")
	}
	var absent *contextRecallRequest
	absent.observeResponseWrite(nil, 0, errors.New("synthetic"))
	absent.finishResponseIdentity()
}

func TestRecallResponseIdentityHTTPDoesNotBufferOrChangeBody(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			release := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: {\"id\":\"synthetic-response-1\",\"choices\":[{\"delta\":{\"content\":\"visible\"}}]}\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
			}))
			defer upstream.Close()
			events := make(chan contextRecallEvent, 1)
			base := newBaseHandler(Config{UpstreamBaseURL: upstream.URL, ContextRecallMetadataOnly: true, ContextRecallResponseIdentity: enabled})
			base.contextRecall = &contextRecallControl{engine: newContextRecallEngine(), emit: func(e contextRecallEvent) { events <- e }}
			base.contextRecall.policy.Store(&contextRecallPolicy{Mode: "apply", Percent: 100, FragmentPercent: 100, RepeatedPercent: 100, StepToolsMode: "apply", StepToolsPercent: 0})
			base.contextRecall.nextReadNS.Store(time.Now().Add(time.Hour).UnixNano())
			server := httptest.NewServer(base)
			defer server.Close()
			request := map[string]any{"model": "kilo/stepfun/step-3.7-flash:free", "stream": true, "max_completion_tokens": 128000,
				"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("synthetic context ", 40000)}},
				"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "description": strings.Repeat("synthetic tool schema ", 1600), "parameters": map[string]any{"type": "object"}}}}}
			b, _ := json.Marshal(request)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(string(b)))
			req.Header.Set("Content-Type", "application/json")
			resp, err := server.Client().Do(req)
			if err != nil {
				close(release)
				t.Fatal("stream request failed")
			}
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			first, err := reader.ReadString('\n')
			close(release)
			if err != nil || !strings.Contains(first, "visible") {
				t.Fatal("first event was buffered")
			}
			rest, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal("stream did not finish")
			}
			body := append([]byte(first), rest...)
			if !strings.Contains(string(body), "[DONE]") {
				t.Fatal("terminal changed")
			}
			select {
			case e := <-events:
				if !e.ToolsEligible || e.ToolsReason != "not_sampled" {
					t.Fatal("fixture did not reach qualified control arm")
				}
				sum := sha256.Sum256(body)
				if enabled {
					if e.ResponseBodySHA256 != hex.EncodeToString(sum[:]) || e.ResponseBodyBytes != int64(len(body)) || e.ResponseWriteFailed || e.ResponseHashLimited || e.ResponseRequestCanceled {
						t.Fatal("client bytes differ from recorded identity")
					}
				} else if e.ResponseBodySHA256 != "" || e.ResponseBodyBytes != 0 {
					t.Fatal("disabled observation changed metadata")
				}
			case <-ctx.Done():
				t.Fatal("qualified event missing")
			}
		})
	}
}
