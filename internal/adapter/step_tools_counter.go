package adapter

// Bounded local tool counting. Counts alone are cached, never schemas.
import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const stepCounterRevision = "5f6244077ac62e04eec3f320501ff8c2b293373a"
const stepCounterSchemaMaxBytes = 524288

type stepToolCountResult struct {
	Tokens    int
	CacheHit  bool
	ComputeUS int64
	CPUUS     int64
	WallUS    int64
}

type stepToolCounterCacheEntry struct {
	tokens int
	at     time.Time
}

type stepToolCounterWorker struct {
	cmd           *exec.Cmd
	input         io.WriteCloser
	output        io.ReadCloser
	reader        *bufio.Reader
	done          chan struct{}
	alive         atomic.Bool
	initWallUS    int64
	initCPUUS     int64
	rssKiB        int64
	rssCurrentKiB int64
	pssCurrentKiB int64
}

func (w *stepToolCounterWorker) stop() {
	if w.alive.Swap(false) {
		_ = w.cmd.Process.Kill()
	}
	_ = w.input.Close()
	_ = w.output.Close()
	select {
	case <-w.done:
	case <-time.After(time.Second):
	}
}

type stepToolCounterPool struct {
	idle         chan *stepToolCounterWorker
	workers      []*stepToolCounterWorker
	mu           sync.Mutex
	cache        map[[32]byte]stepToolCounterCacheEntry
	closed       atomic.Bool
	nextID       atomic.Uint64
	served       atomic.Uint64
	hits         atomic.Uint64
	busy         atomic.Uint64
	failed       atomic.Uint64
	wallUS       atomic.Uint64
	maxWall      atomic.Uint64
	maxCPU       atomic.Int64
	cpuUS        atomic.Uint64
	recovered    atomic.Uint64
	repairFailed atomic.Uint64
	lifetime     context.Context
	cancel       context.CancelFunc
	repair       chan *stepToolCounterWorker
	repairDone   chan struct{}
	restartDelay time.Duration
	command      []string
}

func newStepToolCounterPool(ctx context.Context, command []string) (*stepToolCounterPool, error) {
	if len(command) == 0 {
		return nil, errors.New("counter_command_required")
	}
	lifetime, cancel := context.WithCancel(context.Background())
	p := &stepToolCounterPool{idle: make(chan *stepToolCounterWorker, 2), cache: make(map[[32]byte]stepToolCounterCacheEntry), lifetime: lifetime, cancel: cancel, repair: make(chan *stepToolCounterWorker, 2), repairDone: make(chan struct{}), restartDelay: 5 * time.Second, command: append([]string(nil), command...)}
	go p.recoverWorkers()
	for i := 0; i < 2; i++ {
		w, err := startStepToolCounterWorker(ctx, command)
		if err != nil {
			p.Close()
			return nil, err
		}
		p.workers = append(p.workers, w)
		p.idle <- w
	}
	return p, nil
}

func startStepToolCounterWorker(ctx context.Context, command []string) (*stepToolCounterWorker, error) {
	cmd := exec.Command(command[0], command[1:]...)
	// Offline assets need no provider credentials or inherited Python hooks.
	cmd.Env = []string{"TOKENIZERS_PARALLELISM=false", "PYTHONNOUSERSITE=1", "PYTHONDONTWRITEBYTECODE=1"}
	cmd.Stderr = io.Discard // Worker exceptions must not expose schemas.
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, errors.New("counter_start_failed")
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		_ = input.Close()
		return nil, errors.New("counter_start_failed")
	}
	w := &stepToolCounterWorker{cmd: cmd, input: input, output: output, reader: bufio.NewReaderSize(output, 4096), done: make(chan struct{})}
	if cmd.Start() != nil {
		_ = input.Close()
		_ = output.Close()
		return nil, errors.New("counter_start_failed")
	}
	w.alive.Store(true)
	go func() { _ = cmd.Wait(); w.alive.Store(false); close(w.done) }()
	ready := make(chan bool, 1)
	go func() {
		line, err := w.reader.ReadSlice('\n')
		var event struct {
			Ready          bool   `json:"ready"`
			Revision       string `json:"revision"`
			InitWallUS     int64  `json:"init_wall_us"`
			InitCPUUS      int64  `json:"init_cpu_us"`
			RSSKiB         int64  `json:"rss_peak_kib"`
			RSSCurrentKiB  int64  `json:"rss_kib"`
			PSSCurrentKiB  int64  `json:"pss_kib"`
			ClearTextCache bool   `json:"clear_text_cache"`
		}
		ok := err == nil && json.Unmarshal(line, &event) == nil && event.Ready && event.Revision == stepCounterRevision && event.ClearTextCache
		w.initWallUS, w.initCPUUS, w.rssKiB = event.InitWallUS, event.InitCPUUS, event.RSSKiB
		w.rssCurrentKiB, w.pssCurrentKiB = event.RSSCurrentKiB, event.PSSCurrentKiB
		ready <- ok
	}()
	select {
	case ok := <-ready:
		if !ok {
			w.stop()
			return nil, errors.New("counter_readiness_failed")
		}
	case <-ctx.Done():
		w.stop()
		return nil, errors.New("counter_readiness_deadline")
	}
	return w, nil
}

func (p *stepToolCounterPool) queueRecovery(w *stepToolCounterWorker) {
	select {
	case <-w.done:
	default:
		return
	} // Never start a replacement while the old process lives.
	if p.closed.Load() {
		return
	}
	select {
	case p.repair <- w:
	default:
	}
}

// One supervisor serializes replacements, with a five-second delay and a
// two-second readiness bound. Requests never wait for or trigger a restart loop.
func (p *stepToolCounterPool) recoverWorkers() {
	defer close(p.repairDone)
	for {
		var dead *stepToolCounterWorker
		select {
		case <-p.lifetime.Done():
			return
		case dead = <-p.repair:
		}
		for {
			timer := time.NewTimer(p.restartDelay)
			select {
			case <-p.lifetime.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			ctx, cancel := context.WithTimeout(p.lifetime, 2*time.Second)
			fresh, err := startStepToolCounterWorker(ctx, p.command)
			cancel()
			if err != nil {
				p.repairFailed.Add(1)
				if p.lifetime.Err() != nil {
					return
				}
				continue
			}
			p.mu.Lock()
			registered := false
			if !p.closed.Load() {
				for i, w := range p.workers {
					if w == dead {
						p.workers[i] = fresh
						registered = true
						break
					}
				}
			}
			p.mu.Unlock()
			if !registered {
				fresh.stop()
				break
			}
			select {
			case <-p.lifetime.Done():
				fresh.stop()
				return
			case p.idle <- fresh:
				p.recovered.Add(1)
			}
			break
		}
	}
}

func (p *stepToolCounterPool) Close() {
	if p == nil || p.closed.Swap(true) {
		return
	}
	p.cancel()
	<-p.repairDone // Pending readiness owns and cleans its private child.
	p.mu.Lock()
	workers := append([]*stepToolCounterWorker(nil), p.workers...)
	clear(p.cache)
	p.mu.Unlock()
	for _, w := range workers {
		w.stop()
	}
}

func (p *stepToolCounterPool) Count(ctx context.Context, tools any) (stepToolCountResult, error) {
	started := time.Now()
	deadline, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	if p != nil {
		defer func() {
			elapsed := uint64(time.Since(started).Microseconds())
			p.wallUS.Add(elapsed)
			for old := p.maxWall.Load(); elapsed > old; old = p.maxWall.Load() {
				if p.maxWall.CompareAndSwap(old, elapsed) {
					break
				}
			}
		}()
	}
	if p == nil || p.closed.Load() || ctx.Err() != nil {
		return stepToolCountResult{}, errors.New("counter_unavailable")
	}
	// Bound traversal before Marshal can allocate or visit an oversized schema.
	// The same deadline covers validation, serialization and native counting.
	if !stepCounterSchemaBounded(deadline, tools) {
		return stepToolCountResult{}, errors.New("counter_schema_size_guard")
	}
	body, err := json.Marshal(tools)
	if err != nil || len(body) > stepCounterSchemaMaxBytes {
		return stepToolCountResult{}, errors.New("counter_schema_size_guard")
	}
	if deadline.Err() != nil {
		return stepToolCountResult{}, errors.New("counter_deadline")
	}
	key := sha256.Sum256(body)
	p.mu.Lock()
	entry, hit := p.cache[key]
	if hit && time.Since(entry.at) < time.Hour {
		p.mu.Unlock()
		p.hits.Add(1)
		return stepToolCountResult{Tokens: entry.tokens, CacheHit: true, WallUS: time.Since(started).Microseconds()}, nil
	}
	delete(p.cache, key)
	p.mu.Unlock()
	var w *stepToolCounterWorker
	select {
	case w = <-p.idle:
	default:
		p.busy.Add(1)
		return stepToolCountResult{}, errors.New("counter_busy")
	}
	defer func() {
		if w.alive.Load() && !p.closed.Load() {
			select {
			case p.idle <- w:
			default:
				w.stop()
			}
		}
	}()
	id := p.nextID.Add(1)
	frame, _ := json.Marshal(struct {
		ID    uint64          `json:"id"`
		Tools json.RawMessage `json:"tools"`
	}{id, body})
	frame = append(frame, '\n')
	type reply struct {
		ID        uint64 `json:"id"`
		Tokens    int    `json:"tokens"`
		ComputeUS int64  `json:"compute_us"`
		CPUUS     int64  `json:"cpu_us"`
		Error     string `json:"error"`
	}
	results := make(chan reply, 1)
	go func() {
		var result reply
		if _, err := w.input.Write(frame); err == nil {
			line, err := w.reader.ReadSlice('\n')
			if err == nil {
				_ = json.Unmarshal(line, &result)
			}
		}
		results <- result
	}()
	select {
	case result := <-results:
		if result.ID != id || result.Error != "" || result.Tokens <= 0 || result.Tokens > stepCounterSchemaMaxBytes || result.CPUUS < 0 || p.closed.Load() || deadline.Err() != nil {
			p.failed.Add(1)
			w.stop()
			p.queueRecovery(w)
			return stepToolCountResult{}, errors.New("counter_reply_guard")
		}
		p.mu.Lock()
		if len(p.cache) >= 256 {
			var oldest [32]byte
			var oldestAt time.Time
			for key, value := range p.cache {
				if oldestAt.IsZero() || value.at.Before(oldestAt) {
					oldest, oldestAt = key, value.at
				}
			}
			delete(p.cache, oldest)
		}
		if !p.closed.Load() {
			p.cache[key] = stepToolCounterCacheEntry{result.Tokens, time.Now()}
		}
		p.mu.Unlock()
		p.served.Add(1)
		p.cpuUS.Add(uint64(result.CPUUS))
		for old := p.maxCPU.Load(); result.ComputeUS > old; old = p.maxCPU.Load() {
			if p.maxCPU.CompareAndSwap(old, result.ComputeUS) {
				break
			}
		}
		return stepToolCountResult{Tokens: result.Tokens, ComputeUS: result.ComputeUS, CPUUS: result.CPUUS, WallUS: time.Since(started).Microseconds()}, nil
	case <-deadline.Done():
		p.failed.Add(1)
		w.stop() // Stops native CPU work, not merely the request's wait.
		p.queueRecovery(w)
		return stepToolCountResult{}, errors.New("counter_deadline")
	}
}

// Only ordinary values produced by JSON decoding are accepted. String sizes
// include JSON escapes; deep/cyclic input and custom marshalers fail closed.
func stepCounterSchemaBounded(ctx context.Context, value any) bool {
	bytes, nodes := 0, 0
	add := func(n int) bool {
		bytes += n
		return bytes <= stepCounterSchemaMaxBytes && ctx.Err() == nil
	}
	text := func(s string) bool {
		if !add(2) {
			return false
		}
		for i := 0; i < len(s); {
			if i%4096 == 0 && ctx.Err() != nil {
				return false
			}
			c := s[i]
			n, size := 1, 1
			if c < 0x20 || c == '<' || c == '>' || c == '&' {
				n = 6
			} else if c == '"' || c == '\\' {
				n = 2
			} else if c >= utf8.RuneSelf {
				r, width := utf8.DecodeRuneInString(s[i:])
				size, n = width, width
				if (r == utf8.RuneError && width == 1) || r == '\u2028' || r == '\u2029' {
					n = 6
				}
			}
			if !add(n) {
				return false
			}
			i += size
		}
		return true
	}
	var visit func(any, int) bool
	visit = func(v any, depth int) bool {
		nodes++
		if depth > 64 || nodes > 32768 || ctx.Err() != nil {
			return false
		}
		switch v := v.(type) {
		case nil:
			return add(4)
		case bool:
			return add(5)
		case string:
			return text(v)
		case float64, float32, int, int64, int32, uint, uint64, uint32:
			return add(32)
		case json.Number:
			return len(v) <= 32 && add(len(v))
		case []any:
			if len(v) > 32768 || !add(2+max(0, len(v)-1)) {
				return false
			}
			for _, child := range v {
				if !visit(child, depth+1) {
					return false
				}
			}
			return true
		case map[string]any:
			if len(v) > 32768 || !add(2+max(0, len(v)-1)+len(v)) {
				return false
			}
			for key, child := range v {
				if !text(key) || !visit(child, depth+1) {
					return false
				}
			}
			return true
		default:
			return false
		}
	}
	return visit(value, 0)
}
