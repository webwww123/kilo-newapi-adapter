package adapter

import (
	"context"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Only repair text the host already truncated. Never change an item role,
// tool argument/result, media block, or the host's input/output budget.
func (q *contextRecallRequest) selectTextItems(payload map[string]any, field string, baseline []any) ([]any, bool) {
	if q == nil {
		return nil, false
	}
	q.event.Kind = "text_fragment"
	q.triggered = true
	source, ok := payload[field].([]any)
	if !ok {
		q.event.Reason = "fragment_unsupported"
		return nil, false
	}
	q.event.InputItems = len(source)
	var original map[string]any
	for _, item := range source {
		if m, ok := item.(map[string]any); ok && contextRecallOrdinaryUser(m) {
			original = m
		}
	}
	for i := len(baseline) - 1; i >= 0; i-- {
		m, ok := baseline[i].(map[string]any)
		if !ok || !contextRecallOrdinaryUser(m) {
			continue
		}
		before, _, ok1 := contextFragmentContent(original["content"])
		after, replace, ok2 := contextFragmentContent(m["content"])
		if !ok1 || !ok2 || !contextFragmentSameMetadata(original, m) || !contextFragmentContentMetadata(original["content"], m["content"]) {
			break
		}
		text, applied := q.selectText(before, after)
		if !applied {
			return nil, false
		}
		copyItem := make(map[string]any, len(m))
		for key, value := range m {
			copyItem[key] = value
		}
		copyItem["content"] = replace(text)
		out := append([]any(nil), baseline...)
		out[i] = copyItem
		q.event.CandidateItems = len(out)
		return out, true
	}
	q.event.Reason = "fragment_unsupported"
	return nil, false
}

// A single plain text block is equivalent to a string. Never flatten images,
// tool_result blocks, multiple parts, or native protocol metadata.
func contextFragmentContent(value any) (string, func(string) any, bool) {
	if s, ok := value.(string); ok {
		return s, func(v string) any { return v }, true
	}
	parts, ok := value.([]any)
	if !ok || len(parts) != 1 {
		return "", nil, false
	}
	block, ok := parts[0].(map[string]any)
	if !ok {
		return "", nil, false
	}
	if block["type"] != "text" && block["type"] != "input_text" {
		return "", nil, false
	}
	s, ok := block["text"].(string)
	if !ok {
		return "", nil, false
	}
	return s, func(v string) any {
		copyBlock := make(map[string]any, len(block))
		for k, x := range block {
			copyBlock[k] = x
		}
		copyBlock["text"] = v
		return []any{copyBlock}
	}, true
}

func contextFragmentContentMetadata(a, b any) bool {
	_, replaceA, okA := contextFragmentContent(a)
	_, replaceB, okB := contextFragmentContent(b)
	return okA && okB && reflect.DeepEqual(replaceA(""), replaceB(""))
}

func contextFragmentSameMetadata(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if key != "content" && !reflect.DeepEqual(value, b[key]) {
			return false
		}
	}
	return true
}

func (q *contextRecallRequest) selectText(original, baseline string) (out string, applied bool) {
	if q == nil {
		return "", false
	}
	q.triggered = true
	q.event.Kind = "text_fragment"
	q.event.Budget = q.control.engine.fragmentCost(baseline)
	if !q.fragmentChosen {
		q.event.Reason = "fragment_control"
		return "", false
	}
	start := time.Now()
	defer func() {
		q.event.SelectMS = time.Since(start).Milliseconds()
		if recover() != nil {
			out = ""
			applied = false
			q.event.Applied = false
			q.event.Reason = "fragment_internal_fallback"
		}
	}()
	ctx, trace := q.repeatedContext()
	metric := newContextRecallMetric()
	if q.disableMetric {
		metric = nil
	}
	out, q.event.Reason = q.control.engine.recallTextMetric(ctx, original, baseline, metric)
	q.recordRepeated(trace, q.event.Reason == "fragment_selected" && q.event.Mode == "apply")
	if q.event.Reason != "fragment_selected" || q.event.Mode != "apply" {
		metric.finishFinal(true)
		q.event.setMetric(metric)
		return "", false
	}
	metric.finishFinal(false)
	q.event.setMetric(metric)
	q.event.Applied = true
	return out, true
}

func (r *contextRecallEngine) fragmentCost(value any) int { return contextEstimateAnyTokens(value) }

type contextFragment struct {
	lo, hi int
	terms  map[string]int
	score  float64
}

// The question part of a text is its last paragraph, capped to about 2000
// estimated tokens of suffix using the same edge convention as the host tail.
func contextRecallQuestionStart(source []rune) int {
	end := len(source)
	for end > 0 && unicode.IsSpace(source[end-1]) {
		end--
	}
	start := 0
	for i := end - 2; i >= 0; i-- {
		if source[i] == '\n' && source[i+1] == '\n' {
			start = i + 2
			break
		}
	}
	units := 0
	for i := end - 1; i >= start; i-- {
		if source[i] <= 127 {
			units++
		} else {
			units += 2
		}
		if units > 4000 {
			// A capped paragraph keeps only whole trailing lines: a mid-line
			// cut leaks document words and broken tokens into the query.
			for j := i + 1; j < end; j++ {
				if source[j] == '\n' {
					return j + 1
				}
			}
			return i + 1
		}
	}
	return start
}

func contextRecallQuestionText(text string) string {
	source := []rune(text)
	return string(source[contextRecallQuestionStart(source):])
}

// Score the same overlapping windows as before against the query. Returns the
// fragment list plus the query-term document frequency. A failure reason is
// returned without the "fragment_" prefix for callers outside this file.
func (r *contextRecallEngine) scoreSourceChunks(ctx context.Context, source []rune, lo, hi int, query map[string]int) ([]contextFragment, map[string]int, string) {
	var chunks []contextFragment
	df := map[string]int{}
	for c := lo; c < hi; c += 768 {
		end := min(hi, c+1024)
		terms, _, ok := contextRecallMatchingTerms(ctx, string(source[c:end]), query)
		if !ok || ctx.Err() != nil {
			return nil, nil, "deadline"
		}
		for term := range terms {
			df[term]++
		}
		chunks = append(chunks, contextFragment{lo: c, hi: end, terms: terms})
		if len(chunks) > 16384 {
			return nil, nil, "item_limit"
		}
	}
	for i := range chunks {
		for term, count := range chunks[i].terms {
			// Repeated filler and common instructions must not crowd out a rare
			// path, identifier or Chinese bigram requested at the edges.
			if df[term] > max(2, len(chunks)/8) {
				continue
			}
			chunks[i].score += math.Log(1+float64(len(chunks))/float64(df[term]+1)) * float64(min(count, 2))
		}
	}
	return chunks, df, ""
}

// Pick the highest-scoring non-overlapping chunks while the accumulated
// estimated cost stays within allowance. Picked chunks come back in source
// order for the existing "\n\n" join format.
func (r *contextRecallEngine) pickSourceChunks(source []rune, chunks []contextFragment, allowance, maxPicks int) []contextFragment {
	order := make([]int, len(chunks))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return chunks[order[a]].score > chunks[order[b]].score })
	var picked []contextFragment
	used := 0
	for _, i := range order {
		chunk := chunks[i]
		if chunk.score <= 0 || len(picked) >= maxPicks {
			break
		}
		overlaps := false
		for _, old := range picked {
			if chunk.lo < old.hi && old.lo < chunk.hi {
				overlaps = true
				break
			}
		}
		if overlaps {
			continue
		}
		cost := r.fragmentCost(string(source[chunk.lo:chunk.hi])) + 8
		if used+cost > allowance {
			continue
		}
		picked = append(picked, chunk)
		used += cost
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i].lo < picked[j].lo })
	return picked
}

func contextRecallJoinText(source []rune, head int, picked []contextFragment, tail int) string {
	var middle strings.Builder
	for _, chunk := range picked {
		middle.WriteString("\n\n")
		middle.WriteString(string(source[chunk.lo:chunk.hi]))
	}
	middle.WriteString("\n\n")
	return string(source[:head]) + middle.String() + string(source[tail:])
}

// Subtract picked fragment ranges from a span. The returned sub-ranges are the
// content a trimmed edge would actually lose.
func contextRecallLostRanges(spanLo, spanHi int, picked []contextFragment) [][2]int {
	var out [][2]int
	pos := spanLo
	for _, chunk := range picked {
		if chunk.hi <= spanLo || chunk.lo >= spanHi {
			continue
		}
		lo, hi := max(chunk.lo, spanLo), min(chunk.hi, spanHi)
		if lo > pos {
			out = append(out, [2]int{pos, lo})
		}
		if hi > pos {
			pos = hi
		}
	}
	if spanHi > pos {
		out = append(out, [2]int{pos, spanHi})
	}
	return out
}

// A plain text item can shrink to its edges plus question-matching fragments.
// Tool calls/results, media, multi-part content and non-message roles never
// qualify; pairing and metadata stay untouched.
type contextShrinkPlan struct {
	index       int
	src         []rune
	head, tail  int
	replace     func(string) any
	minimal     string
	minimalCost int
}

func (r *contextRecallEngine) planItemShrink(item any) *contextShrinkPlan {
	m, ok := item.(map[string]any)
	if !ok || contextItemHasToolCall(m) || len(contextAnthropicToolResultIDSet(m)) > 0 {
		return nil
	}
	if role := contextItemRole(m); role != "user" && role != "assistant" {
		return nil
	}
	text, replace, ok := contextFragmentContent(m["content"])
	if !ok || text == "" {
		return nil
	}
	src := []rune(text)
	tail := contextRecallQuestionStart(src)
	head := min(8192, tail)
	if tail-head < 2048 {
		return nil
	}
	minimal := contextRecallJoinText(src, head, nil, tail)
	return &contextShrinkPlan{src: src, head: head, tail: tail, replace: replace, minimal: minimal, minimalCost: contextEstimateAnyTokens(replace(minimal))}
}

// Rebuild the item map with a replacement content value, keeping every other
// key exactly as the protocol sent it.
func contextRecallReplaceContent(item any, content any) any {
	m, ok := item.(map[string]any)
	if !ok {
		return item
	}
	copyItem := make(map[string]any, len(m))
	for key, value := range m {
		copyItem[key] = value
	}
	copyItem["content"] = content
	return copyItem
}

func (r *contextRecallEngine) recallText(ctx context.Context, original, baseline string) (string, string) {
	return r.recallTextMetric(ctx, original, baseline, nil)
}

func (r *contextRecallEngine) recallTextMetric(ctx context.Context, original, baseline string, metric *contextRecallMetric) (string, string) {
	var (
		source, old            []rune
		headKeep, preTailStart int
		split, tailStart       int
		question               map[string]int
		chunks, picked         []contextFragment
		df                     map[string]int
	)
	if len(original) > r.maxBytes {
		return "", "fragment_size_limit"
	}
	if original == baseline || len(baseline) >= len(original) {
		return "", "fragment_not_needed"
	}
	if ctx.Err() != nil {
		return "", "fragment_deadline"
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		return "", "fragment_busy"
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	// Registered after cancel so the metric still sees a live context.
	defer func() {
		r.fillTextMetric(ctx, metric, source, old, headKeep, split, tailStart, preTailStart, question, chunks, picked, df)
	}()
	source, old = []rune(original), []rune(baseline)
	if len(old) < 16384 {
		return "", "fragment_small_budget"
	}
	// Locate the existing prefix/suffix concatenation. Ambiguous repeated text
	// uses a valid split nearest the host's one-quarter prefix convention.
	prefix, suffix := 0, 0
	for prefix < len(old) && old[prefix] == source[prefix] {
		prefix++
	}
	for suffix < len(old) && old[len(old)-1-suffix] == source[len(source)-1-suffix] {
		suffix++
	}
	if prefix+suffix < len(old) {
		return "", "fragment_unknown_baseline"
	}
	split = len(old) / 4
	if split > prefix {
		split = prefix
	}
	if split < len(old)-suffix {
		split = len(old) - suffix
	}
	if split < 8192 || split >= len(old) {
		return "", "fragment_protected_budget"
	}
	tailStart = len(source) - (len(old) - split)
	if tailStart <= split {
		return "", "fragment_not_needed"
	}
	budget := r.fragmentCost(baseline)
	// The retrieval question is only the final paragraph of the message, never
	// the document body the host happened to keep around it.
	questionStart := contextRecallQuestionStart(source)
	headKeep = 8192
	if questionStart <= headKeep {
		return "", "fragment_not_needed"
	}
	// The document ending the baseline kept just before the question is also
	// mandatory: for pasted logs and reports the answer sits at the document
	// end, not inside the question wording. Its size follows the baseline
	// tail, capped at one eighth of the budget so fragments keep the rest.
	preTailStart = questionStart
	if tailStart < questionStart {
		limit := budget / 4 // estimate units: ascii 1 / non-ascii 2
		units := 0
		preTailStart = tailStart
		for i := questionStart - 1; i >= tailStart; i-- {
			if source[i] <= 127 {
				units++
			} else {
				units += 2
			}
			if units > limit {
				preTailStart = i + 1
				break
			}
		}
	}
	questionText := string(source[questionStart:])
	if contextRecallBroadQuery(questionText) {
		return "", "fragment_broad_query"
	}
	question, _, ok := contextRecallTerms(ctx, questionText)
	if !ok {
		return "", "fragment_deadline"
	}
	if len(question) == 0 {
		return "", "fragment_no_query"
	}
	query := make(map[string]int, len(question))
	for term := range question {
		query[term] = 1
	}
	chunks, df, failed := r.scoreSourceChunks(ctx, source, headKeep, preTailStart, query)
	if failed != "" {
		return "", "fragment_" + failed
	}
	scored := false
	for i := range chunks {
		if chunks[i].score > 0 {
			scored = true
			break
		}
	}
	if !scored {
		// A task statement lives in the opening paragraph of some documents.
		// Only when the question finds nothing do those opening terms score;
		// document text elsewhere never reaches the query.
		headTerms, _, ok := contextRecallTerms(ctx, string(source[:min(2048, headKeep)]))
		if !ok {
			return "", "fragment_deadline"
		}
		for term := range headTerms {
			query[term] = 1
		}
		chunks, df, failed = r.scoreSourceChunks(ctx, source, headKeep, preTailStart, query)
		if failed != "" {
			return "", "fragment_" + failed
		}
	}
	var lineFallbackRequired []repeatedLocation
	var lineFallbackGuard *repeatedScanner
	if contextRepeatedEnabled(ctx) {
		contextRepeatedAttempt(ctx)
		scanQuestion := questionText
		if runes := []rune(scanQuestion); len(runes) > 512 {
			scanQuestion = string(runes[len(runes)-512:])
		}
		lineFallbackRequired, lineFallbackGuard = contextRepeatedFragments(ctx, scanQuestion, source, headKeep, preTailStart, df, chunks)
		if len(lineFallbackRequired) > 0 {
			contextRepeatedMatch(ctx)
		}
	}
	// When the latest message itself exceeds budget the recalled middle may use
	// up to half of it — and never more than what the mandatory edges leave.
	allowance := budget / 2
	if rem := budget - r.fragmentCost(contextRecallJoinText(source, headKeep, nil, preTailStart)) - 16; rem < allowance {
		allowance = rem
	}
	if allowance <= 0 {
		return "", "fragment_budget"
	}
	picked = r.pickSourceChunks(source, chunks, allowance, 256)
	for _, hit := range lineFallbackRequired {
		covered := false
		for _, chunk := range picked {
			if chunk.lo <= hit.lo && chunk.hi >= hit.hi {
				covered = true
			}
		}
		if !covered {
			return "", "fragment_repeated_query_budget"
		}
	}
	if len(picked) == 0 {
		return "", "fragment_no_evidence"
	}
	// The mandatory edges are the opening 8192 runes and the complete question
	// part. Everything else the host kept may still be dropped, but never the
	// last copy of a rare question term: occurrences preserved in the kept head
	// or in a picked fragment are not being evicted at all.
	keptDoc := map[string]int{}
	if terms, _, ok := contextRecallMatchingTerms(ctx, string(source[:headKeep]), question); ok {
		for term := range terms {
			keptDoc[term] = 1
		}
	}
	// The mandatory tail always survives; a term still present there is never
	// being evicted and must not block recovery.
	if terms, _, ok := contextRecallMatchingTerms(ctx, string(source[preTailStart:]), question); ok {
		for term := range terms {
			keptDoc[term] = 1
		}
	}
	for _, chunk := range picked {
		for term := range chunk.terms {
			keptDoc[term] = 1
		}
	}
	var lost [][2]int
	lost = append(lost, contextRecallLostRanges(headKeep, split, picked)...)
	lost = append(lost, contextRecallLostRanges(tailStart, preTailStart, picked)...)
	for _, span := range lost {
		terms, _, ok := contextRecallMatchingTerms(ctx, string(source[span[0]:span[1]]), question)
		if !ok {
			return "", "fragment_deadline"
		}
		for term := range terms {
			if keptDoc[term] == 0 && df[term] <= max(2, len(chunks)/8) {
				return "", "fragment_protected_evidence"
			}
		}
		if lineFallbackGuard != nil && !lineFallbackGuard.allowPrefixLoss(source, span[0], span[1]) {
			return "", "fragment_protected_repeated_evidence"
		}
	}
	out := contextRecallJoinText(source, headKeep, picked, preTailStart)
	if ctx.Err() != nil {
		return "", "fragment_deadline"
	}
	if r.fragmentCost(out) > budget || r.fragmentCost(contextRecallJoinText(source, 0, picked, len(source))) > allowance+16 {
		return "", "fragment_budget"
	}
	return out, "fragment_selected"
}
