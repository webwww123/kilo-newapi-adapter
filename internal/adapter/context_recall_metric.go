package adapter

// Numbers-only accounting of how much of the trimmed-out content the
// retrieval question actually referenced. The metrics reuse the term tables
// and chunk scans the selector already built; the only new work is counting
// bounded query terms inside the cut region and the final output's changed
// parts. Baseline term counts are derived by subtracting the dropped region
// from the whole instead of rescanning the kept text. No term or user content
// is ever logged.
import (
	"context"
	"sort"
	"strings"
	"time"
)

const (
	// When more question terms qualify, only the first 64 (sorted) are
	// evaluated so the extra work stays bounded on very large inputs.
	contextRecallMetricTermCap = 64
	// Hard wall budget for all metric work in one request, independent of
	// the selection deadline. Anything not computed in time stays at -1 and
	// never affects the request.
	contextRecallMetricBudget = 80 * time.Millisecond
)

type contextRecallMetric struct {
	q         int
	cut       int
	baseHit   int
	finalHit  int
	tailKept  int
	budgetUse int
	ms        int64
	spent     time.Duration
	begin     time.Time
	occQ      map[string]int
	cutSet    []string
	dead      bool
	parent    context.Context

	// Item-path state retained for the final count: which items the selector
	// kept or replaced, and the per-item term tables of anchor and tool-owner
	// items (their text is excluded from the owning turn's table).
	items    []any
	turns    []contextRecallTurn
	selected []bool
	replaced map[int]any
	perItem  map[int]map[string]int

	// Fragment-path state: head/tail term tables and the picked chunks are
	// enough to rebuild the final count without touching the output text.
	frag       bool
	fragHead   map[string]int
	fragTail   map[string]int
	fragPicked []contextFragment
}

func newContextRecallMetric() *contextRecallMetric {
	return &contextRecallMetric{q: -1, cut: -1, baseHit: -1, finalHit: -1, tailKept: -1, budgetUse: -1}
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

func contextRecallJoinedText(items []any) string {
	var b strings.Builder
	for _, item := range items {
		b.WriteString(contextRecallText(item))
		b.WriteByte('\n')
	}
	return b.String()
}

// The metric scans under its own deadline, detached from the selection
// context so a long selection cannot starve it; the same 80ms envelope still
// bounds every phase of metric work combined.
func (m *contextRecallMetric) scanContext(parent context.Context) (context.Context, context.CancelFunc) {
	if m.parent == nil {
		m.parent = parent
	}
	base := m.parent
	if base == nil {
		base = context.Background()
	}
	remaining := contextRecallMetricBudget - m.spent
	if !m.begin.IsZero() {
		remaining -= time.Since(m.begin)
	}
	if remaining < time.Millisecond {
		remaining = time.Millisecond
	}
	return context.WithTimeout(context.WithoutCancel(base), remaining)
}

// Record the final selection. When nothing was applied the final text is the
// baseline, so the count equals the baseline self-check.
func (m *contextRecallMetric) finishFinal(sameAsBaseline bool) {
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
	ctx, cancel := m.scanContext(m.parent)
	defer cancel()
	cutQuery := make(map[string]int, len(m.cutSet))
	for _, term := range m.cutSet {
		cutQuery[term] = 1
	}
	fin := make(map[string]int, len(m.cutSet))
	if m.frag {
		for _, table := range []map[string]int{m.fragHead, m.fragTail} {
			for term, count := range table {
				fin[term] += count
			}
		}
		for _, chunk := range m.fragPicked {
			for term, count := range chunk.terms {
				fin[term] += count
			}
		}
	} else {
		for i := range m.turns {
			turn := &m.turns[i]
			allKept, anyKept, changed := true, false, false
			for _, index := range turn.indexes {
				if turn.tool && index == turn.owner {
					continue
				}
				if !m.selected[index] {
					allKept = false
					continue
				}
				anyKept = true
				if _, ok := m.replaced[index]; ok {
					changed = true
				}
			}
			switch {
			case allKept && !changed:
				for term, count := range turn.terms {
					fin[term] += count
				}
			case anyKept:
				for _, index := range turn.indexes {
					if (turn.tool && index == turn.owner) || !m.selected[index] {
						continue
					}
					item := m.items[index]
					if rep, ok := m.replaced[index]; ok {
						item = rep
					}
					terms, _, ok := contextRecallMatchingTerms(ctx, contextRecallText(item), cutQuery)
					if !ok || m.late() {
						return
					}
					for term, count := range terms {
						fin[term] += count
					}
				}
			}
		}
		for index, terms := range m.perItem {
			if !m.selected[index] {
				continue
			}
			if rep, ok := m.replaced[index]; ok {
				reTerms, _, ok := contextRecallMatchingTerms(ctx, contextRecallText(rep), cutQuery)
				if !ok || m.late() {
					return
				}
				terms = reTerms
			}
			for term, count := range terms {
				fin[term] += count
			}
		}
	}
	hit := 0
	for _, term := range m.cutSet {
		if fin[term] > m.occQ[term] {
			hit++
		}
	}
	m.finalHit = hit
}

// Shared tail of both metric fills: of the evaluated effective question
// terms, keep those with occurrences beyond the question span (present in the
// original content) that the baseline did not keep. cutOcc holds the term
// counts of the dropped region only, so the baseline check never rescans the
// kept text.
func (m *contextRecallMetric) cutAgainstBaseline(eval []string, occ, cutOcc, occQ map[string]int) {
	var cut []string
	for _, term := range eval {
		if m.late() {
			return
		}
		if occ[term] <= occQ[term] {
			continue
		}
		if occ[term]-cutOcc[term] > occQ[term] {
			continue
		}
		cut = append(cut, term)
	}
	m.cut = len(cut)
	m.cutSet = cut
	// A cut term the baseline still shows beyond the question invalidates the
	// subtraction accounting; recounting it is the baseline self-check.
	hit := 0
	for _, term := range cut {
		if occ[term]-cutOcc[term] > occQ[term] {
			hit++
		}
	}
	m.baseHit = hit
}

// Item-path metric: term occurrences per turn (owner text of tool turns is
// excluded by the selector, so it is counted here together with anchor-only
// system/developer items) minus the occurrences inside the question span.
func (r *contextRecallEngine) fillItemsMetric(parent context.Context, m *contextRecallMetric, items []any, turns []contextRecallTurn, query, df map[string]int, questionSpan string, baselineMask, selected []bool, replaced map[int]any, repCost map[int]int, costs []int, budget int, tail50 []int, baseEst int, applied bool) {
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
	// Numbers-only guards: of the baseline's last fifty positions, how many
	// the output keeps, and how much of the budget the output estimate uses.
	// On any fallback the sent output is the baseline itself, so the tail
	// count is whole and the estimate is the baseline estimate.
	if len(tail50) > 0 && budget > 0 {
		if !applied {
			m.tailKept = len(tail50)
			m.budgetUse = baseEst * 100 / budget
		} else {
			keptTail := 0
			for _, index := range tail50 {
				if index < len(selected) && selected[index] {
					keptTail++
				}
			}
			m.tailKept = keptTail
			est := 0
			for i := range items {
				if !selected[i] {
					continue
				}
				if c, ok := repCost[i]; ok {
					est += c
				} else {
					est += costs[i]
				}
			}
			m.budgetUse = est * 100 / budget
		}
	}
	if len(query) == 0 || len(turns) == 0 || df == nil || len(baselineMask) == 0 || questionSpan == "" {
		return
	}
	ctx, cancel := m.scanContext(parent)
	defer cancel()
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
	eval := eff
	if len(eval) > contextRecallMetricTermCap {
		eval = eval[:contextRecallMetricTermCap]
	}
	evalQuery := make(map[string]int, len(eval))
	for _, term := range eval {
		evalQuery[term] = 1
	}
	occQ, _, ok := contextRecallMatchingTerms(ctx, questionSpan, query)
	if !ok || m.late() {
		return
	}
	m.occQ = occQ
	occ := make(map[string]int, len(query))
	cutOcc := make(map[string]int, len(eval))
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
	perItem := make(map[int]map[string]int)
	for i, item := range items {
		role := contextItemRole(item)
		if role != "system" && role != "developer" && !owner[i] {
			continue
		}
		terms, _, ok := contextRecallMatchingTerms(ctx, contextRecallText(item), query)
		if !ok || m.late() {
			return
		}
		perItem[i] = terms
		for term, count := range terms {
			occ[term] += count
			if !baselineMask[i] {
				cutOcc[term] += count
			}
		}
	}
	// Whole dropped turns contribute their existing term table; only turns
	// straddling the kept boundary rescan, and only their dropped members.
	for i := range turns {
		turn := &turns[i]
		allCut, anyCut := true, false
		for _, index := range turn.indexes {
			if turn.tool && index == turn.owner {
				continue
			}
			if baselineMask[index] {
				allCut = false
			} else {
				anyCut = true
			}
		}
		switch {
		case allCut:
			for term, count := range turn.terms {
				cutOcc[term] += count
			}
		case anyCut:
			for _, index := range turn.indexes {
				if (turn.tool && index == turn.owner) || baselineMask[index] {
					continue
				}
				terms, _, ok := contextRecallMatchingTerms(ctx, contextRecallText(items[index]), evalQuery)
				if !ok || m.late() {
					return
				}
				for term, count := range terms {
					cutOcc[term] += count
				}
			}
		}
	}
	m.items, m.turns, m.selected, m.replaced = items, turns, selected, replaced
	m.perItem = perItem
	m.cutAgainstBaseline(eval, occ, cutOcc, occQ)
}

// Fragment-path metric: chunk term counts cover the scored middle; the kept
// head and the preserved tail (which includes the question) are counted once
// more so every region of the original contributes. The dropped region is the
// source span between the baseline prefix and suffix, so its counts come from
// the same chunk tables plus at most two boundary chunk rescans.
func (r *contextRecallEngine) fillTextMetric(parent context.Context, m *contextRecallMetric, source, old []rune, headKeep, split, tailStart, preTailStart int, question map[string]int, chunks, picked []contextFragment, df map[string]int) {
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
	if len(question) == 0 || df == nil || len(source) == 0 {
		return
	}
	ctx, cancel := m.scanContext(parent)
	defer cancel()
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
	eval := eff
	if len(eval) > contextRecallMetricTermCap {
		eval = eval[:contextRecallMetricTermCap]
	}
	evalQuery := make(map[string]int, len(eval))
	for _, term := range eval {
		evalQuery[term] = 1
	}
	m.occQ = question
	occ := make(map[string]int, len(question))
	cutOcc := make(map[string]int, len(eval))
	headTerms, _, ok := contextRecallMatchingTerms(ctx, string(source[:headKeep]), question)
	if !ok || m.late() {
		return
	}
	for term, count := range headTerms {
		occ[term] += count
	}
	tailTerms, _, ok := contextRecallMatchingTerms(ctx, string(source[preTailStart:]), question)
	if !ok || m.late() {
		return
	}
	for term, count := range tailTerms {
		occ[term] += count
	}
	for _, chunk := range chunks {
		for term, count := range chunk.terms {
			occ[term] += count
		}
	}
	if tailStart > split {
		for _, chunk := range chunks {
			if chunk.hi <= split || chunk.lo >= tailStart {
				continue
			}
			if chunk.lo >= split && chunk.hi <= tailStart {
				for term, count := range chunk.terms {
					cutOcc[term] += count
				}
				continue
			}
			lo, hi := max(chunk.lo, split), min(chunk.hi, tailStart)
			if lo >= hi {
				continue
			}
			terms, _, ok := contextRecallMatchingTerms(ctx, string(source[lo:hi]), evalQuery)
			if !ok || m.late() {
				return
			}
			for term, count := range terms {
				cutOcc[term] += count
			}
		}
		if lo := max(split, preTailStart); lo < tailStart {
			terms, _, ok := contextRecallMatchingTerms(ctx, string(source[lo:tailStart]), evalQuery)
			if !ok || m.late() {
				return
			}
			for term, count := range terms {
				cutOcc[term] += count
			}
		}
	} else {
		// Early exits before the baseline split was proven leave the drop
		// region unknown; a single term-table pass over the baseline text is
		// still far cheaper than the old per-term substring scans.
		base, _, ok := contextRecallMatchingTerms(ctx, string(old), evalQuery)
		if !ok || m.late() {
			return
		}
		for _, term := range eval {
			cutOcc[term] = occ[term] - base[term]
		}
	}
	m.frag = true
	m.fragHead, m.fragTail, m.fragPicked = headTerms, tailTerms, picked
	m.cutAgainstBaseline(eval, occ, cutOcc, question)
}
