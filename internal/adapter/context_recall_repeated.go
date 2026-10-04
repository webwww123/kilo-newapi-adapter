package adapter

// Bounded opt-in fallback. Used after the original scorer finds no evidence.
// Common query terms can still identify a rare statement among verbatim repeated
// lines. This does not infer an answer, summarize, or resolve conflicting values.
import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"
)

type repeatedLocation struct{ doc, lo, hi int }
type repeatedLine struct {
	count     int
	locations []repeatedLocation
}
type repeatedScanner struct {
	ctx   context.Context
	query map[string]int
	lines map[string]*repeatedLine
	valid bool
}

// Each selection keeps its own rollout decision and metadata. Missing context
// opts out, so direct engine calls cannot silently enable the new fallback.
type contextRepeatedKey struct{}
type contextRepeatedTraceKey struct{}
type contextRepeatedTrace struct{ attempted, matched bool }

func contextRepeatedEnabled(ctx context.Context) bool {
	value, _ := ctx.Value(contextRepeatedKey{}).(bool)
	return value
}

func contextRepeatedAttempt(ctx context.Context) {
	if trace, _ := ctx.Value(contextRepeatedTraceKey{}).(*contextRepeatedTrace); trace != nil {
		trace.attempted = true
	}
}

func contextRepeatedMatch(ctx context.Context) {
	if trace, _ := ctx.Value(contextRepeatedTraceKey{}).(*contextRepeatedTrace); trace != nil {
		trace.matched = true
	}
}

func (q *contextRecallRequest) repeatedContext() (context.Context, *contextRepeatedTrace) {
	trace := &contextRepeatedTrace{}
	ctx := context.WithValue(q.ctx, contextStepToolsGuardKey{}, q.event.ToolsApplied)
	ctx = context.WithValue(ctx, contextRepeatedKey{}, q.repeatedChosen)
	return context.WithValue(ctx, contextRepeatedTraceKey{}, trace), trace
}

func (q *contextRecallRequest) recordRepeated(trace *contextRepeatedTrace, applied bool) {
	q.event.RepeatedAttempted = q.event.RepeatedAttempted || trace.attempted
	q.event.RepeatedMatched = q.event.RepeatedMatched || trace.matched
	q.event.RepeatedApplied = applied && trace.matched
}

func newRepeatedScanner(ctx context.Context, question string, df map[string]int, n int, fragment bool) *repeatedScanner {
	question = strings.TrimSpace(question)
	if at := strings.LastIndexByte(question, '\n'); at >= 0 {
		question = question[at+1:]
	}
	if utf8.RuneCountInString(question) > 512 || contextRecallBroadQuery(question) {
		return nil
	}
	terms, _, ok := contextRecallTerms(ctx, question)
	if !ok || len(terms) > 32 {
		return nil
	}
	for term := range terms {
		common := df[term]*3 >= n
		if fragment {
			common = df[term] > max(2, n/8)
		}
		if !common {
			delete(terms, term)
		}
	}
	if len(terms) == 0 {
		return nil
	}
	return &repeatedScanner{ctx: ctx, query: terms, lines: map[string]*repeatedLine{}, valid: true}
}

func (s *repeatedScanner) add(text string, doc, base int) {
	if s == nil || !s.valid {
		return
	}
	offset := base
	for len(text) > 0 {
		if s.ctx.Err() != nil {
			s.valid = false
			return
		}
		line, rest, found := strings.Cut(text, "\n")
		n := utf8.RuneCountInString(line)
		if n > 512 {
			// A long unexamined line might contain another value; decline the
			// fallback rather than claim that all rare occurrences were covered.
			s.valid = false
			return
		}
		key := strings.TrimSpace(line)
		if key != "" && n <= 512 {
			if record := s.lines[key]; record != nil {
				record.count++
				if record.count <= 2 {
					record.locations = append(record.locations, repeatedLocation{doc, offset, offset + n})
				}
			} else {
				matches, _, ok := contextRecallMatchingTerms(s.ctx, key, s.query)
				if !ok {
					s.valid = false
					return
				}
				if len(matches) > 0 {
					if len(s.lines) >= 256 {
						s.valid = false
						return
					}
					s.lines[key] = &repeatedLine{count: 1, locations: []repeatedLocation{{doc, offset, offset + n}}}
				}
			}
		}
		offset += n + 1
		text = rest
		if !found {
			break
		}
	}
}

func (s *repeatedScanner) rare() []repeatedLocation {
	if s == nil || !s.valid || s.ctx.Err() != nil {
		return nil
	}
	hasRepetition := false
	var out []repeatedLocation
	for _, line := range s.lines {
		if line.count >= 8 {
			hasRepetition = true
		}
		if line.count <= 2 {
			out = append(out, line.locations...)
		}
	}
	// Never silently pick one of many distinct values. Return unchanged if the
	// full set of rare occurrences cannot fit the existing eight-fragment cap.
	if !hasRepetition || len(out) == 0 || len(out) > 8 {
		return nil
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].doc != out[j].doc {
			return out[i].doc < out[j].doc
		}
		return out[i].lo < out[j].lo
	})
	return out
}

func contextRepeatedFragments(ctx context.Context, question string, source []rune, lo, hi int, df map[string]int, chunks []contextFragment) ([]repeatedLocation, *repeatedScanner) {
	for _, chunk := range chunks {
		if chunk.score > 0 {
			return nil, nil
		}
	}
	s := newRepeatedScanner(ctx, question, df, len(chunks), true)
	if s == nil {
		return nil, nil
	}
	// Ignore incomplete boundary lines; cutting a repeated line must not turn it
	// into a rare statement. All returned offsets refer to original source text.
	for lo < hi && source[lo] != '\n' {
		lo++
	}
	lo++
	for hi > lo && source[hi-1] != '\n' {
		hi--
	}
	if lo >= hi {
		return nil, nil
	}
	s.add(string(source[lo:hi]), 0, lo)
	rare := s.rare()
	scores := make(map[int]float64)
	for _, hit := range rare {
		found := false
		for i := range chunks {
			if chunks[i].lo <= hit.lo && chunks[i].hi >= hit.hi {
				scores[i]++
				found = true
			}
		}
		if !found {
			return nil, nil
		}
	}
	for i, score := range scores {
		chunks[i].score += score
	}
	if len(rare) == 0 {
		return nil, nil
	}
	return rare, s
}

// Frequent terms were filtered out by the original prefix guard. The fallback
// must still preserve a distinct existing statement containing those terms.
// Only exact copies of a line repeated at least eight times are expendable.
func (s *repeatedScanner) allowPrefixLoss(source []rune, lo, hi int) bool {
	if lo >= hi {
		return true
	}
	for lo > 0 && source[lo-1] != '\n' {
		lo--
	}
	for hi < len(source) && source[hi-1] != '\n' {
		hi++
	}
	text := string(source[lo:hi])
	for len(text) > 0 {
		if s.ctx.Err() != nil {
			return false
		}
		line, rest, found := strings.Cut(text, "\n")
		key := strings.TrimSpace(line)
		if key != "" {
			if utf8.RuneCountInString(line) > 512 {
				return false
			}
			if repeated := s.lines[key]; repeated == nil || repeated.count < 8 {
				terms, _, ok := contextRecallMatchingTerms(s.ctx, key, s.query)
				if !ok || len(terms) > 0 {
					return false
				}
			}
		}
		text = rest
		if !found {
			break
		}
	}
	return true
}

func contextRepeatedTurns(ctx context.Context, items []any, turns []contextRecallTurn, df map[string]int) []int {
	question := ""
	for i := len(items) - 1; i >= 0; i-- {
		if contextRecallOrdinaryUser(items[i]) {
			question = contextRecallText(items[i])
			break
		}
	}
	s := newRepeatedScanner(ctx, question, df, len(turns), false)
	if s == nil {
		return nil
	}
	for i, turn := range turns {
		if i == len(turns)-1 || turn.tool {
			continue
		}
		for _, index := range turn.indexes {
			s.add(contextRecallText(items[index]), i, 0)
		}
	}
	rare := s.rare()
	var required []int
	for _, hit := range rare {
		if len(required) == 0 || required[len(required)-1] != hit.doc {
			required = append(required, hit.doc)
		}
		turns[hit.doc].score++
	}
	return required
}
