//go:build recall_offline

package adapter

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"testing"
)

// Diverse identifier-heavy records exercise allocation and tokenization costs
// that repeated natural-language padding cannot expose. These are byte sizes,
// not a claim about any provider's tokenizer.
func recallScaleFixture(size int, style string) recallFixture {
	f := makeRecallFixture("old_named_file", "chat", size/2)
	if style == "repeated" {
		return f
	}
	rng := rand.New(rand.NewSource(92731))
	for _, item := range f.Payload["messages"].([]any) {
		m := item.(map[string]any)
		value, ok := m["content"].(string)
		if !ok || len(value) < 512 {
			continue
		}
		var b strings.Builder
		b.Grow(len(value) + 100)
		b.WriteString(value[:256])
		for b.Len() < len(value) {
			fmt.Fprintf(&b, "entry_%016x object_%016x checksum_%016x = %d;\n", rng.Uint64(), rng.Uint64(), rng.Uint64(), rng.Intn(100000))
		}
		m["content"] = b.String()
	}
	return f
}

func TestRecallScaleOffline(t *testing.T) {
	path := os.Getenv("RECALL_SCALE_REPORT")
	if path == "" {
		t.Skip("set RECALL_SCALE_REPORT for bounded size experiments")
	}
	var rows []map[string]any
	for _, style := range []string{"repeated", "identifiers"} {
		for _, size := range []int{2 << 20, 4 << 20, 8 << 20, 13 << 20} {
			f := recallScaleFixture(size, style)
			raw := recallJSON(t, f.Payload)
			profile := recallProfile(f.Payload, "chat", 128000)
			for n := 0; n < 3; n++ {
				runtime.GC()
				old, _, a := measuredRecall(t, raw, "chat", profile, nil)
				out, why, b := measuredRecall(t, raw, "chat", profile, newRecallExperiment())
				validateRecallSelection(t, raw, out, "chat", 128000, why == "selected")
				if why != "selected" && string(out) != string(old) {
					t.Fatal("scale fallback changed baseline")
				}
				row := map[string]any{"style": style, "input_bytes": len(raw), "run": n, "reason": why, "baseline": a, "candidate": b}
				rows = append(rows, row)
				t.Logf("style=%s bytes=%d run=%d reason=%s wall_ms=%.1f/%.1f allocation_mib=%.1f/%.1f", style, len(raw), n, why, a.WallMS, b.WallMS, a.AllocatedMB, b.AllocatedMB)
			}
		}
	}
	b, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
