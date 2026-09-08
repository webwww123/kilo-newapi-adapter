package adapter

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestProxyDeliversStreamBeforeUpstreamCompletes(t *testing.T) {
	for _, tc := range []struct {
		name, path, request, firstEvent, lastEvent, terminalMarker string
	}{
		{"chat", "/v1/chat/completions", `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}],"stream":true}`,
			`{"choices":[{"delta":{"content":"first visible text"}}]}`, `[DONE]`, `[DONE]`},
		{"messages", "/v1/messages", `{"model":"kilo/public-model","messages":[{"role":"user","content":"hi"}],"stream":true,"max_tokens":16}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"first visible text"}}`, `{"type":"message_stop"}`, `"type":"message_stop"`},
		{"responses", "/v1/responses", `{"model":"kilo/public-model","input":"hi","stream":true}`,
			`{"type":"response.output_text.delta","delta":"first visible text"}`, `{"type":"response.completed","response":{"status":"completed"}}`, `"type":"response.completed"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Keep the producer blocked after a small flushed event. The client must
			// receive it before producer completion, below net/http's buffer size.
			release := make(chan struct{})
			var once sync.Once
			releaseUpstream := func() { once.Do(func() { close(release) }) }
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "data: "+tc.firstEvent+"\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, "data: "+tc.lastEvent+"\n\n")
			}))
			defer upstream.Close()
			adapter := httptest.NewServer(NewHandler(Config{UpstreamBaseURL: upstream.URL, RequestLogDir: t.TempDir()}))
			defer adapter.Close()
			defer releaseUpstream()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, adapter.URL+tc.path, strings.NewReader(tc.request))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := adapter.Client().Do(req)
			if err != nil {
				t.Fatalf("small stream event was buffered until completion: %v", err)
			}
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			line, err := reader.ReadString('\n')
			if err != nil || !strings.Contains(line, "first visible text") {
				t.Fatalf("first event=%q err=%v", line, err)
			}
			releaseUpstream()
			rest, err := io.ReadAll(reader)
			if err != nil || !strings.Contains(string(rest), tc.terminalMarker) {
				t.Fatalf("completion lost: body=%s err=%v", rest, err)
			}
		})
	}
}

func TestLoggingStreamFlushKeepsExplicitJSONBuffer(t *testing.T) {
	buffer := newLingJSONBufferedWriter()
	status, size := http.StatusOK, int64(0)
	w := &loggingResponseWriter{ResponseWriter: buffer, status: &status, bytesWritten: &size}
	const body = "data: {\"content\":\"unvalidated\"}\n\n"
	copyEventStreamWithNormalizeAndCapture(w, strings.NewReader(body), nil, "", "/chat/completions")
	if !strings.Contains(buffer.body.String(), "unvalidated") || size != int64(buffer.body.Len()) {
		t.Fatalf("explicit validation buffer or logging changed: body=%s size=%d", buffer.body.String(), size)
	}
}
