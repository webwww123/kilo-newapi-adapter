package adapter

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestRepeatedLineAmbiguityAndLimits(t *testing.T) {
	makeScanner := func(text string) []repeatedLocation {
		s := newRepeatedScanner(context.Background(), "What is FETCH_LIMIT?", map[string]int{"fetch_limit": 100}, 100, true)
		s.add(text, 0, 0)
		return s.rare()
	}
	noise := strings.Repeat("FETCH_LIMIT pending review.\n", 10)
	if got := makeScanner(noise + "FETCH_LIMIT = 401\nFETCH_LIMIT = 809\n"); len(got) != 2 {
		t.Fatal("must keep both conflicting values", got)
	}
	if got := makeScanner(noise); len(got) != 0 {
		t.Fatal("absent value manufactured", got)
	}
	if got := makeScanner("FETCH_LIMIT = 401\n"); len(got) != 0 {
		t.Fatal("fallback requires repeated noise", got)
	}
	for _, count := range []int{9, 257} {
		var b strings.Builder
		b.WriteString(noise)
		for i := 0; i < count; i++ {
			fmt.Fprintf(&b, "FETCH_LIMIT = %d\n", i)
		}
		if got := makeScanner(b.String()); len(got) != 0 {
			t.Fatal("silently chose partial value set", count)
		}
	}
	if got := makeScanner(noise + "FETCH_LIMIT = 401\n" + strings.Repeat("x", 513) + " FETCH_LIMIT = 809\n"); len(got) != 0 {
		t.Fatal("ignored unexamined long statement")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s := newRepeatedScanner(ctx, "FETCH_LIMIT?", map[string]int{"fetch_limit": 100}, 100, true); s != nil {
		t.Fatal("cancelled scan admitted")
	}
}

func TestRepeatedLineCannotReturnPartialFragmentSet(t *testing.T) {
	source := []rune(strings.Repeat("FETCH_LIMIT pending review.\n", 10) + "FETCH_LIMIT = 301\nFETCH_LIMIT = 901\nQuestion: FETCH_LIMIT?\n")
	// There is room for only the first of two rare statements in this synthetic
	// chunk list. Aborting must leave scores untouched, not return a partial set.
	end := strings.Index(string(source), "FETCH_LIMIT = 901")
	chunks := []contextFragment{{lo: 0, hi: end}}
	got, _ := contextRepeatedFragments(context.Background(), "Question: FETCH_LIMIT?\n", source, 0, len(source)-len("Question: FETCH_LIMIT?\n"), map[string]int{"fetch_limit": 20}, chunks)
	if len(got) != 0 || chunks[0].score != 0 {
		t.Fatal("partial fallback mutated original score", got, chunks)
	}
}

func TestRepeatedLineProtectsExistingValues(t *testing.T) {
	s := newRepeatedScanner(context.Background(), "FETCH_LIMIT?", map[string]int{"fetch_limit": 100}, 100, true)
	s.add(strings.Repeat("FETCH_LIMIT pending review.\n", 10)+"FETCH_LIMIT = 999\n", 0, 0)
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"FETCH_LIMIT pending review.\n", true},
		{"FETCH_LIMIT = 431\n", false},
		{"FETCH_LIMIT = 999\n", false},
		{"Some unrelated punctuation.\n", true},
	} {
		// Removal begins inside a line: compare the entire original statement.
		runes := []rune(tc.text)
		if got := s.allowPrefixLoss(runes, 2, len(runes)-2); got != tc.want {
			t.Fatal(tc.text, got)
		}
	}
}

func TestRepeatedLineFullPathPreservesExistingValue(t *testing.T) {
	noise := strings.Repeat("FETCH_LIMIT pending review.\n", 20000)
	question := "What is FETCH_LIMIT?\n"
	// Keep the two query edges free of cut-off filler tokens so this fixture
	// exercises the common-term fallback, rather than the older rare-term guard.
	head := question + strings.Repeat(" ", 2048-len(question))
	tail := strings.Repeat(" ", 2048-len(question)) + question
	original := head + noise[2048:39960] + "\nFETCH_LIMIT = prefix-431\n" + noise[39960:270000] + "\nFETCH_LIMIT = middle-999\n" + noise[270000:] + tail
	baseline := trimStringPreservingEdgesToContextBudget(original, 80000)
	if !strings.Contains(baseline, "prefix-431") || strings.Contains(baseline, "middle-999") {
		t.Fatal("invalid prefix protection fixture")
	}
	out, why := newContextRecallEngine().recallText(context.WithValue(context.Background(), contextRepeatedKey{}, true), original, baseline)
	if why != "fragment_selected" || !strings.Contains(out, "prefix-431") || !strings.Contains(out, "middle-999") {
		t.Fatal("evicted an existing distinct value", why)
	}
}
