package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type RequestLogConfig struct {
	Dir       string
	Retention time.Duration
	BodyLimit int64
	now       func() time.Time
}

type requestLogger struct {
	cfg RequestLogConfig
	mu  sync.Mutex
}

type requestLogEntry struct {
	Timestamp     string          `json:"timestamp"`
	Method        string          `json:"method"`
	Path          string          `json:"path"`
	Query         string          `json:"query,omitempty"`
	UpstreamPath  string          `json:"upstream_path,omitempty"`
	Status        int             `json:"status"`
	DurationMS    int64           `json:"duration_ms"`
	BytesWritten  int64           `json:"bytes_written"`
	RemoteAddr    string          `json:"remote_addr,omitempty"`
	UserAgent     string          `json:"user_agent,omitempty"`
	Request       requestLogBody  `json:"request"`
	Response      responseLogBody `json:"response"`
	AdapterError  string          `json:"adapter_error,omitempty"`
	UpstreamError string          `json:"upstream_error,omitempty"`
}

type requestLogBody struct {
	ContentType string `json:"content_type,omitempty"`
	Model       string `json:"model,omitempty"`
	BodyBytes   int64  `json:"body_bytes"`
	Truncated   bool   `json:"truncated"`
	Body        string `json:"body,omitempty"`
}

type responseLogBody struct {
	ContentType string `json:"content_type,omitempty"`
	BodyBytes   int64  `json:"body_bytes"`
	Truncated   bool   `json:"truncated"`
	Body        string `json:"body,omitempty"`
}

type loggableRequest struct {
	Method       string
	Path         string
	Query        string
	UpstreamPath string
	RemoteAddr   string
	UserAgent    string
	ContentType  string
	Body         []byte
}

type loggableResponse struct {
	Status        int
	Duration      time.Duration
	BytesWritten  int64
	ContentType   string
	Body          []byte
	Truncated     bool
	AdapterError  string
	UpstreamError string
}

func newRequestLogger(cfg RequestLogConfig) *requestLogger {
	if cfg.now == nil {
		cfg.now = time.Now
	}
	if cfg.Retention <= 0 {
		cfg.Retention = 48 * time.Hour
	}
	if cfg.BodyLimit <= 0 {
		cfg.BodyLimit = 1 << 20
	}
	return &requestLogger{cfg: cfg}
}

func (l *requestLogger) record(req loggableRequest, resp loggableResponse) error {
	if l == nil {
		return nil
	}
	if strings.TrimSpace(l.cfg.Dir) == "" {
		return errors.New("request log directory is empty")
	}
	now := l.cfg.now()
	entry := requestLogEntry{
		Timestamp:     now.Format(time.RFC3339Nano),
		Method:        req.Method,
		Path:          req.Path,
		Query:         req.Query,
		UpstreamPath:  req.UpstreamPath,
		Status:        resp.Status,
		DurationMS:    resp.Duration.Milliseconds(),
		BytesWritten:  resp.BytesWritten,
		RemoteAddr:    req.RemoteAddr,
		UserAgent:     req.UserAgent,
		AdapterError:  resp.AdapterError,
		UpstreamError: resp.UpstreamError,
		Request: requestLogBody{
			ContentType: req.ContentType,
			Model:       extractModel(req.Body),
			BodyBytes:   int64(len(req.Body)),
			Truncated:   int64(len(req.Body)) > l.cfg.BodyLimit,
			Body:        safeBodyString(limitBytes(req.Body, l.cfg.BodyLimit)),
		},
		Response: responseLogBody{
			ContentType: resp.ContentType,
			BodyBytes:   resp.BytesWritten,
			Truncated:   resp.Truncated,
			Body:        safeBodyString(resp.Body),
		},
	}

	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	if err := os.MkdirAll(l.cfg.Dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(l.cfg.Dir, "requests-"+now.Format("2006-01-02")+".jsonl")

	l.mu.Lock()
	defer l.mu.Unlock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

func (l *requestLogger) cleanup() error {
	if l == nil || strings.TrimSpace(l.cfg.Dir) == "" {
		return nil
	}
	entries, err := os.ReadDir(l.cfg.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cutoff := l.cfg.now().Add(-l.cfg.Retention)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "requests-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(l.cfg.Dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

type captureBuffer struct {
	limit     int64
	buf       bytes.Buffer
	total     int64
	truncated bool
}

func newCaptureBuffer(limit int64) *captureBuffer {
	if limit <= 0 {
		limit = 1 << 20
	}
	return &captureBuffer{limit: limit}
}

func (b *captureBuffer) Write(data []byte) (int, error) {
	b.total += int64(len(data))
	remaining := b.limit - int64(b.buf.Len())
	if remaining > 0 {
		if int64(len(data)) > remaining {
			b.buf.Write(data[:remaining])
			b.truncated = true
		} else {
			b.buf.Write(data)
		}
	} else if len(data) > 0 {
		b.truncated = true
	}
	return len(data), nil
}

func (b *captureBuffer) Bytes() []byte {
	if b == nil {
		return nil
	}
	return b.buf.Bytes()
}

func (b *captureBuffer) Truncated() bool {
	return b != nil && b.truncated
}

func (b *captureBuffer) Total() int64 {
	if b == nil {
		return 0
	}
	return b.total
}

func limitBytes(data []byte, limit int64) []byte {
	if limit <= 0 || int64(len(data)) <= limit {
		return data
	}
	return data[:limit]
}

func safeBodyString(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	if utf8.Valid(data) {
		return string(data)
	}
	return string(bytes.ToValidUTF8(data, []byte("\uFFFD")))
}

func extractModel(body []byte) string {
	if len(bytes.TrimSpace(body)) == 0 {
		return ""
	}
	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	return payload.Model
}

func copyStreamingWithCapture(w http.ResponseWriter, r io.Reader, capture *captureBuffer) {
	flusher, _ := w.(http.Flusher)
	buffer := make([]byte, 32*1024)
	for {
		n, err := r.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			if capture != nil {
				_, _ = capture.Write(chunk)
			}
			_, _ = w.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
