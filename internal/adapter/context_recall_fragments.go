package adapter

import (
	"context"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"
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
	out, q.event.Reason = q.control.engine.recallText(ctx, original, baseline)
	q.recordRepeated(trace, q.event.Reason == "fragment_selected" && q.event.Mode == "apply")
	if q.event.Reason != "fragment_selected" || q.event.Mode != "apply" {
		return "", false
	}
	q.event.Applied = true
	return out, true
}

func (r *contextRecallEngine) fragmentCost(value any) int { return contextEstimateAnyTokens(value) }

type contextFragment struct {
	lo, hi int
	terms  map[string]int
	score  float64
}

func (r *contextRecallEngine) recallText(ctx context.Context, original, baseline string) (string, string) {
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
	source, old := []rune(original), []rune(baseline)
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
	split := len(old) / 4
	if split > prefix {
		split = prefix
	}
	if split < len(old)-suffix {
		split = len(old) - suffix
	}
	if split < 8192 || split >= len(old) {
		return "", "fragment_protected_budget"
	}
	tailStart := len(source) - (len(old) - split)
	if tailStart <= split {
		return "", "fragment_not_needed"
	}
	queryText := string(source[:min(2048, split)]) + "\n" + string(source[max(tailStart, len(source)-2048):])
	if contextRecallBroadQuery(queryText) {
		return "", "fragment_broad_query"
	}
	query, _, ok := contextRecallTerms(ctx, queryText)
	if !ok {
		return "", "fragment_deadline"
	}
	if len(query) == 0 {
		return "", "fragment_no_query"
	}
	var chunks []contextFragment
	df := map[string]int{}
	for lo := split; lo < tailStart; lo += 768 {
		hi := min(tailStart, lo+1024)
		terms, _, ok := contextRecallMatchingTerms(ctx, string(source[lo:hi]), query)
		if !ok || ctx.Err() != nil {
			return "", "fragment_deadline"
		}
		for term := range terms {
			df[term]++
		}
		chunks = append(chunks, contextFragment{lo: lo, hi: hi, terms: terms})
		if len(chunks) > 16384 {
			return "", "fragment_item_limit"
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
	var lineFallbackRequired []repeatedLocation
	var lineFallbackGuard *repeatedScanner
	if contextRepeatedEnabled(ctx) {
		contextRepeatedAttempt(ctx)
		lineFallbackRequired, lineFallbackGuard = contextRepeatedFragments(ctx, source, split, tailStart, df, chunks)
		if len(lineFallbackRequired) > 0 {
			contextRepeatedMatch(ctx)
		}
	}
	sort.SliceStable(chunks, func(i, j int) bool { return chunks[i].score > chunks[j].score })
	budget := r.fragmentCost(baseline)
	allowance := budget / 5
	var selected []contextFragment
	used := 0
	for _, chunk := range chunks {
		if chunk.score <= 0 || len(selected) >= 8 {
			break
		}
		overlaps := false
		for _, old := range selected {
			overlaps = overlaps || chunk.lo < old.hi && old.lo < chunk.hi
		}
		if overlaps {
			continue
		}
		cost := r.fragmentCost(string(source[chunk.lo:chunk.hi])) + 8
		if used+cost > allowance {
			continue
		}
		selected = append(selected, chunk)
		used += cost
	}

	for _, hit := range lineFallbackRequired {
		covered := false
		for _, chunk := range selected {
			if chunk.lo <= hit.lo && chunk.hi >= hit.hi {
				covered = true
			}
		}
		if !covered {
			return "", "fragment_repeated_query_budget"
		}
	}
	if len(selected) == 0 {
		return "", "fragment_no_evidence"
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].lo < selected[j].lo })
	var middle strings.Builder
	for _, chunk := range selected {
		middle.WriteString("\n\n")
		middle.WriteString(string(source[chunk.lo:chunk.hi]))
	}
	middle.WriteString("\n\n")
	if r.fragmentCost(middle.String()) > allowance {
		return "", "fragment_share_limit"
	}
	tail := string(source[tailStart:])
	compose := func(n int) string { return string(source[:n]) + middle.String() + tail }
	// The complete pre-existing tail and first 8192 runes are mandatory.
	// Borrow only from the end of the old prefix, at most one fifth of budget.
	lo, hi := 8192, split
	if r.fragmentCost(compose(lo)) > budget {
		return "", "fragment_protected_budget"
	}
	for lo < hi {
		if ctx.Err() != nil {
			return "", "fragment_deadline"
		}
		mid := (lo + hi + 1) / 2
		if r.fragmentCost(compose(mid)) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}

	if lineFallbackGuard != nil && !lineFallbackGuard.allowPrefixLoss(source, lo, split) {
		return "", "fragment_protected_repeated_evidence"
	}
	out := compose(lo)
	if ctx.Err() != nil {
		return "", "fragment_deadline"
	}
	// Do not evict existing evidence for the same query to add older evidence.
	lost, _, ok := contextRecallMatchingTerms(ctx, string(source[lo:split]), query)
	if !ok {
		return "", "fragment_deadline"
	}
	for term := range lost {
		if df[term] <= max(2, len(chunks)/8) {
			return "", "fragment_protected_evidence"
		}
	}
	if r.fragmentCost(out) > budget || r.fragmentCost(string(source[lo:split])) > allowance+16 {
		return "", "fragment_budget"
	}
	return out, "fragment_selected"
}
