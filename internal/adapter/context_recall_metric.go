package adapter

// Numbers-only accounting of how much of the trimmed-out content the
// retrieval question actually referenced. The metrics reuse the term tables
// and chunk scans the selector already built; the only new work is counting
// bounded query terms inside the baseline and final texts with an exact
// token-boundary check. No term or user content is ever logged.
import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	// When more question terms qualify, only the first 64 (sorted) are
	// evaluated so the extra work stays bounded on very large inputs.
	contextRecallMetricTermCap = 64
	// Hard wall budget for all metric work in one request. Anything not
	// computed in time stays at -1 and never affects the request.
	contextRecallMetricBudget = 24 * time.Millisecond
)

type contextRecallMetric struct {
	q        int
	cut      int
	baseHit  int
	finalHit int
	ms       int64
	spent    time.Duration
	begin    time.Time
	occQ     map[string]int
	cutSet   []string
	dead     bool
}

func newContextRecallMetric() *contextRecallMetric {
	return &contextRecallMetric{q: -1, cut: -1, baseHit: -1, finalHit: -1}
}

func (m *contextRecallMetric) enter() {
	if m != nil {
		m.begin = time.Now()
	}
}

func (m *contextRecallMetric) leave() {
	if m != nil {
		m.spent += time.Since(m.begin)
		m.ms = m.spent.Milliseconds()
	}
}

func (m *contextRecallMetric) late() bool {
	if m == nil || m.dead {
		return true
	}
	if m.spent+time.Since(m.begin) > contextRecallMetricBudget {
		m.dead = true
		return true
	}
	return false
}

func (m *contextRecallMetric) fail() {
	if m != nil {
		m.dead = true
	}
}

func contextRecallWordByte(c byte) bool {
	return c < 128 && (unicode.IsLetter(rune(c)) || unicode.IsDigit(rune(c)) || contextRecallTrimByte(c))
}

func contextRecallTrimByte(c byte) bool {
	return c == '.' || c == '_' || c == '/' || c == '-'
}

// Report whether an already-lowercased term occurs more than limit times
// inside already-lowercased text, stopping at limit+1 hits. The token rules
// match contextRecallMatchingTerms: an ASCII word occurrence is a token only
// when the surrounding word run is pure trimmable ._/- edges, and a Han
// bigram occurrence is plain adjacency.
func contextRecallTermExceeds(lowered, term string, limit int) bool {
	if term == "" || lowered == "" {
		return false
	}
	if rs := []rune(term); len(rs) == 2 && unicode.Is(unicode.Han, rs[0]) && unicode.Is(unicode.Han, rs[1]) {
		n, at := 0, 0
		for {
			i := strings.Index(lowered[at:], term)
			if i < 0 {
				return false
			}
			n++
			if n > limit {
				return true
			}
			at += i + 1
		}
	}
	n, at := 0, 0
	for {
		i := strings.Index(lowered[at:], term)
		if i < 0 {
			return false
		}
		s, e := at+i, at+i+len(term)
		j := s
		for j > 0 && contextRecallTrimByte(lowered[j-1]) {
			j--
		}
		k := e
		for k < len(lowered) && contextRecallTrimByte(lowered[k]) {
			k++
		}
		if (j == 0 || !contextRecallWordByte(lowered[j-1])) && (k == len(lowered) || !contextRecallWordByte(lowered[k])) {
			n++
			if n > limit {
				return true
			}
		}
		at = s + 1
	}
}

func contextRecallJoinedText(items []any) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString(contextRecallText(item))
		b.WriteByte('\n')
	}
	return b.String()
}

// Shared tail of both metric fills: of the evaluated effective question
// terms, keep those with occurrences beyond the question span (present in the
// original content) that the baseline did not keep.
func (m *contextRecallMetric) cutAgainstBaseline(eval []string, occ, occQ map[string]int, baselineText string) {
	base := strings.ToLower(baselineText)
	var cut []string
	for _, term := range eval {
		if m.late() {
			return
		}
		if occ[term] <= occQ[term] {
			continue
		}
		if contextRecallTermExceeds(base, term, occQ[term]) {
			continue
		}
		cut = append(cut, term)
	}
	m.cut = len(cut)
	m.cutSet = cut
	hit := 0
	for _, term := range cut {
		if contextRecallTermExceeds(base, term, occQ[term]) {
			hit++
		}
	}
	m.baseHit = hit
}

// Record the final text actually sent upstream. When nothing was applied the
// final text is the baseline, so the count equals the baseline self-check.
func (m *contextRecallMetric) finishFinal(finalText string, sameAsBaseline bool) {
	if m == nil {
		return
	}
	m.enter()
	defer m.leave()
	defer func() {
		if recover() != nil {
			m.fail()
		}
	}()
	if m.cut < 0 || m.dead {
		return
	}
	if sameAsBaseline || m.cut == 0 {
		m.finalHit = m.baseHit
		return
	}
	final := strings.ToLower(finalText)
	hit := 0
	for _, term := range m.cutSet {
		if m.late() {
			return
		}
		if contextRecallTermExceeds(final, term, m.occQ[term]) {
			hit++
		}
	}
	m.finalHit = hit
}

// Item-path metric: term occurrences per turn (owner text of tool turns is
// excluded by the selector, so it is counted here together with anchor-only
// system/developer items) minus the occurrences inside the question span.
func (r *contextRecallEngine) fillItemsMetric(ctx context.Context, m *contextRecallMetric, items []any, turns []contextRecallTurn, query, df map[string]int, questionSpan string, baseline []any) {
	if m == nil {
		return
	}
	m.enter()
	defer m.leave()
	defer func() {
		if recover() != nil {
			m.fail()
		}
	}()
	if len(query) == 0 || len(turns) == 0 || df == nil || len(baseline) == 0 || questionSpan == "" {
		return
	}
	eff := make([]string, 0, len(query))
	for term := range query {
		if df[term]*3 < len(turns) {
			eff = append(eff, term)
		}
	}
	sort.Strings(eff)
	m.q = len(eff)
	if len(eff) == 0 {
		m.cut, m.baseHit = 0, 0
		return
	}
	occQ, _, ok := contextRecallMatchingTerms(ctx, questionSpan, query)
	if !ok || m.late() {
		return
	}
	m.occQ = occQ
	occ := make(map[string]int, len(query))
	for _, turn := range turns {
		for term, count := range turn.terms {
			occ[term] += count
		}
	}
	owner := map[int]bool{}
	for _, turn := range turns {
		if turn.tool && turn.owner >= 0 {
			owner[turn.owner] = true
		}
	}
	for i, item := range items {
		role := contextItemRole(item)
		if role != "system" && role != "developer" && !owner[i] {
			continue
		}
		terms, _, ok := contextRecallMatchingTerms(ctx, contextRecallText(item), query)
		if !ok || m.late() {
			return
		}
		for term, count := range terms {
			occ[term] += count
		}
	}
	eval := eff
	if len(eval) > contextRecallMetricTermCap {
		eval = eval[:contextRecallMetricTermCap]
	}
	m.cutAgainstBaseline(eval, occ, occQ, contextRecallJoinedText(baseline))
}

// Fragment-path metric: chunk term counts cover the scored middle; the kept
// head and the preserved tail (which includes the question) are counted once
// more so every region of the original contributes.
func (r *contextRecallEngine) fillTextMetric(ctx context.Context, m *contextRecallMetric, source, old []rune, headKeep, preTailStart int, question map[string]int, chunks []contextFragment, df map[string]int) {
	if m == nil {
		return
	}
	m.enter()
	defer m.leave()
	defer func() {
		if recover() != nil {
			m.fail()
		}
	}()
	if len(question) == 0 || df == nil || len(source) == 0 || len(old) == 0 {
		return
	}
	limit := max(2, len(chunks)/8)
	eff := make([]string, 0, len(question))
	for term := range question {
		if df[term] <= limit {
			eff = append(eff, term)
		}
	}
	sort.Strings(eff)
	m.q = len(eff)
	if len(eff) == 0 {
		m.cut, m.baseHit = 0, 0
		return
	}
	m.occQ = question
	occ := make(map[string]int, len(question))
	for _, chunk := range chunks {
		for term, count := range chunk.terms {
			occ[term] += count
		}
	}
	for _, span := range [][2]int{{0, headKeep}, {preTailStart, len(source)}} {
		if span[1] <= span[0] {
			continue
		}
		terms, _, ok := contextRecallMatchingTerms(ctx, string(source[span[0]:span[1]]), question)
		if !ok || m.late() {
			return
		}
		for term, count := range terms {
			occ[term] += count
		}
	}
	eval := eff
	if len(eval) > contextRecallMetricTermCap {
		eval = eval[:contextRecallMetricTermCap]
	}
	m.cutAgainstBaseline(eval, occ, question, string(old))
}
