package adapter

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Bounded local selection. No model calls, persistent index or background work.
type contextRecallEngine struct {
	slots    chan struct{}
	maxBytes int
	maxItems int
	timeout  time.Duration
}

func newContextRecallEngine() *contextRecallEngine {
	return &contextRecallEngine{slots: make(chan struct{}, 2), maxBytes: 12 << 20, maxItems: 16384, timeout: 250 * time.Millisecond}
}

type contextRecallTurn struct {
	indexes []int
	owner   int
	tool    bool
	terms   map[string]int
	length  int
	score   float64
}

func (r *contextRecallEngine) choose(ctx context.Context, payload map[string]any, protocol string, rawBytes, budget int, baseline ...[]any) ([]any, string) {
	return r.chooseMetric(ctx, payload, protocol, rawBytes, budget, nil, baseline...)
}

func (r *contextRecallEngine) chooseMetric(ctx context.Context, payload map[string]any, protocol string, rawBytes, budget int, metric *contextRecallMetric, baseline ...[]any) (kept []any, reason string) {
	var (
		items        []any
		turns        []contextRecallTurn
		query, df    map[string]int
		questionSpan string
		baselineMask []bool
		selected     []bool
		replaced     map[int]any
	)
	if rawBytes > r.maxBytes {
		return nil, "size_limit"
	}
	if ctx.Err() != nil {
		return nil, "cancelled"
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		return nil, "busy"
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	// Registered after cancel so the metric still sees a live context.
	defer func() {
		r.fillItemsMetric(ctx, metric, items, turns, query, df, questionSpan, baselineMask, selected, replaced)
	}()
	field := contextRecallField(protocol)
	items, ok := payload[field].([]any)
	if !ok || len(items) == 0 {
		return nil, "unsupported_shape"
	}
	if len(items) > r.maxItems {
		return nil, "item_limit"
	}
	if protocol == "responses" && (contextResponsesHasExternalState(payload) || !contextResponsesItemsSafeToTrim(items)) {
		return nil, "unsupported_state"
	}
	if !contextToolHistorySafeToTrim(items) || !contextRecallSupported(items) {
		return nil, "unsupported_structure"
	}
	if budget < 512 {
		return nil, "fixed_fields_overflow"
	}
	costs := make([]int, len(items))
	totalCost := 0
	for i, item := range items {
		if ctx.Err() != nil {
			return nil, "deadline"
		}
		costs[i] = contextEstimateAnyTokens(item) + 4
		totalCost += costs[i]
	}
	if totalCost <= budget {
		return nil, "not_needed"
	}
	turns, anchors := contextRecallUnits(items)
	if len(turns) == 0 {
		return nil, "no_turns"
	}
	if len(baseline) != 1 {
		return nil, "baseline_missing"
	}
	baselineMask = make([]bool, len(items))
	baselineAt := 0
	for i, item := range items {
		if baselineAt < len(baseline[0]) && reflect.DeepEqual(item, baseline[0][baselineAt]) {
			baselineMask[i] = true
			baselineAt++
		}
	}
	if baselineAt != len(baseline[0]) {
		return nil, "baseline_invalid"
	}
	selected = make([]bool, len(items))
	chosen := make([]bool, len(turns))
	replaced = map[int]any{}
	repCost := map[int]int{}
	repBase := map[int]int{}
	used := 0
	for _, i := range anchors {
		if contextRecallOrdinaryUser(items[i]) {
			continue
		}
		selected[i] = true
		used += costs[i]
	}
	// The retrieval question is only the final paragraph of the latest user
	// message. Earlier user turns still contribute their statement edges.
	query = make(map[string]int)
	userTurns := 0
	queryTurnLimit := 1
	for i := len(items) - 1; i >= 0 && userTurns < queryTurnLimit; i-- {
		if !contextRecallOrdinaryUser(items[i]) {
			continue
		}
		userTurns++
		full := contextRecallText(items[i])
		text := full
		if userTurns == 1 {
			question := contextRecallQuestionText(full)
			questionSpan = question
			if contextRecallBroadQuery(question) {
				return nil, "broad_query"
			}
			lower := strings.ToLower(strings.TrimSpace(question))
			if strings.HasPrefix(lower, "继续") || strings.HasPrefix(lower, "continue") || strings.HasPrefix(lower, "go on") || strings.HasPrefix(lower, "proceed") {
				queryTurnLimit = 6
			}
			text = question
			if runes := []rune(full); len(runes) > 2048 {
				text += "\n" + string(runes[:2048])
			}
		} else if len(text) > 8192 {
			text = text[:4096] + " " + text[len(text)-4096:]
		}
		terms, _, ok := contextRecallTerms(ctx, text)
		if !ok {
			return nil, "deadline"
		}
		for term := range terms {
			query[term] = 1
		}
	}
	if len(query) == 0 {
		return nil, "no_query_terms"
	}
	add := func(i, limit int) bool {
		additional := 0
		for _, index := range turns[i].indexes {
			if !selected[index] {
				additional += costs[index]
			}
		}
		if chosen[i] || used+additional > limit {
			return false
		}
		for _, index := range turns[i].indexes {
			selected[index] = true
		}
		chosen[i] = true
		used += additional
		return true
	}
	// Protect the last ten source items expanded to complete atomic units, the
	// final recall turns, and the anchor task statements. A protected plain text
	// message too large to keep whole shrinks to its edges plus the fragments
	// most relevant to the question; tool units are never split or shrunk.
	required := make([]bool, len(turns))
	for i := len(turns) - 1; i >= 0 && i >= len(turns)-2; i-- {
		required[i] = true
	}
	for i, turn := range turns {
		for _, index := range turn.indexes {
			if index >= len(items)-10 {
				required[i] = true
			}
		}
	}
	requiredIdx := map[int]bool{}
	for i, turn := range turns {
		if !required[i] {
			continue
		}
		for _, index := range turn.indexes {
			requiredIdx[index] = true
		}
	}
	for _, i := range anchors {
		if contextRecallOrdinaryUser(items[i]) {
			requiredIdx[i] = true
		}
	}
	var protectedIdx []int
	for index := range requiredIdx {
		protectedIdx = append(protectedIdx, index)
	}
	sort.Ints(protectedIdx)
	requiredCost := 0
	for _, index := range protectedIdx {
		if !selected[index] {
			requiredCost += costs[index]
		}
	}
	keepWhole := func() bool {
		for i := range turns {
			if required[i] && !add(i, budget) {
				return false
			}
		}
		for _, index := range protectedIdx {
			if selected[index] {
				continue
			}
			selected[index] = true
			used += costs[index]
		}
		return used <= budget
	}
	if used+requiredCost <= budget {
		if !keepWhole() {
			return nil, "mandatory_turn_too_large"
		}
	} else {
		plans := map[int]*contextShrinkPlan{}
		minRequired := used
		for _, index := range protectedIdx {
			if selected[index] {
				continue
			}
			if plan := r.planItemShrink(items[index]); plan != nil && plan.minimalCost+4 < costs[index] {
				plan.index = index
				plans[index] = plan
				minRequired += plan.minimalCost + 4
			} else {
				minRequired += costs[index]
			}
		}
		if minRequired > budget {
			return nil, "mandatory_turn_too_large"
		}
		// Whole protection stays free of the old-evidence share up to the same
		// four fifths it could cost in the minimal layout.
		allowanceCap := budget - (budget-minRequired)/5
		if used+requiredCost <= allowanceCap {
			if !keepWhole() {
				return nil, "mandatory_turn_too_large"
			}
		} else {
			for _, index := range protectedIdx {
				if selected[index] {
					continue
				}
				if plan := plans[index]; plan != nil {
					replaced[index] = contextRecallReplaceContent(items[index], plan.replace(plan.minimal))
					repCost[index] = plan.minimalCost + 4
					repBase[index] = plan.minimalCost + 4
					used += repCost[index]
				} else {
					used += costs[index]
				}
				selected[index] = true
			}
			for i := range turns {
				if required[i] {
					chosen[i] = true
				}
			}
			if used > budget {
				return nil, "mandatory_turn_too_large"
			}
			// Grow shrunk messages with their most question-relevant fragments,
			// ranked across all of them, while preserving the evidence share.
			if slack := budget - used; slack > 0 && len(plans) > 0 {
				pool := slack - slack/5
				var order []int
				for index := range plans {
					order = append(order, index)
				}
				sort.Ints(order)
				type candidate struct {
					plan  *contextShrinkPlan
					chunk contextFragment
				}
				var all []candidate
				pickedBy := map[int][]contextFragment{}
				for _, index := range order {
					plan := plans[index]
					chunks, _, failed := r.scoreSourceChunks(ctx, plan.src, plan.head, plan.tail, query)
					if failed != "" {
						return nil, "deadline"
					}
					for _, chunk := range chunks {
						if chunk.score > 0 {
							all = append(all, candidate{plan, chunk})
						}
					}
				}
				sort.SliceStable(all, func(a, b int) bool { return all[a].chunk.score > all[b].chunk.score })
				for _, entry := range all {
					if pool <= 0 {
						break
					}
					overlaps := false
					for _, old := range pickedBy[entry.plan.index] {
						if entry.chunk.lo < old.hi && old.lo < entry.chunk.hi {
							overlaps = true
							break
						}
					}
					if overlaps {
						continue
					}
					cost := r.fragmentCost(string(entry.plan.src[entry.chunk.lo:entry.chunk.hi])) + 8
					if cost > pool {
						continue
					}
					pickedBy[entry.plan.index] = append(pickedBy[entry.plan.index], entry.chunk)
					pool -= cost
				}
				for _, index := range order {
					picked := pickedBy[index]
					if len(picked) == 0 {
						continue
					}
					plan := plans[index]
					sort.Slice(picked, func(i, j int) bool { return picked[i].lo < picked[j].lo })
					text := contextRecallJoinText(plan.src, plan.head, picked, plan.tail)
					item := contextRecallReplaceContent(items[index], plan.replace(text))
					cost := contextEstimateAnyTokens(item) + 4
					used += cost - repCost[index]
					repCost[index] = cost
					replaced[index] = item
				}
			}
		}
	}
	if used > budget {
		return nil, "mandatory_turn_too_large"
	}
	// Fixed instructions and task statements are not discretionary history.
	// Older evidence may use at most one fifth of the remaining capacity. A
	// shrunk anchor frees the same share its original cost would have kept.
	anchorCharge := 0
	for _, i := range anchors {
		if c, ok := repBase[i]; ok {
			anchorCharge += c
		} else if c, ok := repCost[i]; ok {
			anchorCharge += c
		} else {
			anchorCharge += costs[i]
		}
	}
	oldAllowance := (budget - anchorCharge) / 5
	recentLimit := budget - oldAllowance
	if used > recentLimit {
		recentLimit = used
	}
	baselineUnit := func(i int) bool {
		if len(turns[i].indexes) == 0 {
			return false
		}
		for _, index := range turns[i].indexes {
			if !baselineMask[index] {
				return false
			}
		}
		return true
	}
	for i := len(turns) - 1; i >= 0; i-- {
		if chosen[i] || !baselineUnit(i) {
			continue
		}
		if !add(i, recentLimit) {
			break
		}
	}
	queryTerms := make([]string, 0, len(query))
	for term := range query {
		queryTerms = append(queryTerms, term)
	}
	sort.Strings(queryTerms)
	df = make(map[string]int)
	totalLength := 0
	for i := range turns {
		if ctx.Err() != nil {
			return nil, "deadline"
		}
		var text strings.Builder
		for _, index := range turns[i].indexes {
			// The owning task remains in the output for interpretation, but must
			// not make every tool round match all of that task's query terms.
			if turns[i].tool && index == turns[i].owner {
				continue
			}
			text.WriteString(contextRecallText(items[index]))
			text.WriteByte('\n')
		}
		terms, length, ok := contextRecallMatchingTerms(ctx, text.String(), query)
		if !ok {
			return nil, "deadline"
		}
		turns[i].terms, turns[i].length = terms, length
		totalLength += length
		for term := range terms {
			if query[term] > 0 {
				df[term]++
			}
		}
	}
	avg := math.Max(1, float64(totalLength)/float64(len(turns)))
	ranked := make([]int, 0, len(turns))
	hasPriorEvidence := false
	for i := range turns {
		if ctx.Err() != nil {
			return nil, "deadline"
		}
		for _, term := range queryTerms {
			if df[term]*3 >= len(turns) {
				continue
			}
			tf := float64(turns[i].terms[term])
			if tf == 0 {
				continue
			}
			idf := math.Log(1 + (float64(len(turns)-df[term])+0.5)/(float64(df[term])+0.5))
			turns[i].score += idf * tf * 2.2 / (tf + 1.2*(0.25+0.75*float64(turns[i].length)/avg))
		}
		if turns[i].score > 0 && i < len(turns)-1 {
			hasPriorEvidence = true
		}
		if turns[i].score > 0 && !chosen[i] {
			ranked = append(ranked, i)
		}
	}

	var lineFallbackRequired []int
	if !hasPriorEvidence {
		if contextRepeatedEnabled(ctx) {
			contextRepeatedAttempt(ctx)
			lineFallbackRequired = contextRepeatedTurns(ctx, items, turns, df)
			if len(lineFallbackRequired) > 0 {
				contextRepeatedMatch(ctx)
			}
		}
		if len(lineFallbackRequired) == 0 {
			return nil, "no_recall_evidence"
		}
		for _, i := range lineFallbackRequired {
			if !chosen[i] {
				ranked = append(ranked, i)
			}
		}
	}

	sort.SliceStable(ranked, func(a, b int) bool {
		i, j := ranked[a], ranked[b]
		if turns[i].score == turns[j].score {
			return i > j
		}
		return turns[i].score > turns[j].score
	})
	cutoff := len(items) - 10
	if cutoff < 0 {
		cutoff = 0
	}
	oldAdded := 0
	// Required task statements and complete recent tool units can already have
	// brought back older items absent from the baseline. Charge them before
	// choosing optional evidence, so a fitting smaller unit can still win.
	// Mandatory members charge at their protected floor; fragments grown inside
	// them came from the slack pool and are not discretionary old evidence.
	charge := func(index int) int {
		if c, ok := repBase[index]; ok {
			return c
		}
		if c, ok := repCost[index]; ok {
			return c
		}
		return costs[index]
	}
	for index, kept := range selected {
		if kept && !baselineMask[index] && index < cutoff {
			oldAdded += charge(index)
		}
	}
	if oldAdded > oldAllowance {
		return nil, "old_share_limit"
	}
	recalled := false
	for _, i := range ranked {
		addition := 0
		for _, index := range turns[i].indexes {
			if !selected[index] && !baselineMask[index] && index < cutoff {
				addition += charge(index)
			}
		}
		if addition == 0 {
			continue
		}
		if oldAdded+addition <= oldAllowance && add(i, budget) {
			oldAdded += addition
			recalled = true
			continue
		}
		// A scored old message too large for the evidence share can still come
		// back as its edges plus question-matching fragments. Units without a
		// shrinkable plain text member, or whose shrink saves nothing, are left
		// out exactly as before.
		var members []int
		plans := map[int]*contextShrinkPlan{}
		for _, index := range turns[i].indexes {
			if selected[index] {
				continue
			}
			members = append(members, index)
			if baselineMask[index] || index >= cutoff {
				continue
			}
			if plan := r.planItemShrink(items[index]); plan != nil && plan.minimalCost+4 < costs[index] {
				plan.index = index
				plans[index] = plan
			}
		}
		if len(plans) == 0 {
			continue
		}
		usedDelta, oldDelta := 0, 0
		for _, index := range members {
			cost := costs[index]
			if plan := plans[index]; plan != nil {
				cost = plan.minimalCost + 4
			}
			usedDelta += cost
			if !baselineMask[index] && index < cutoff {
				oldDelta += cost
			}
		}
		if oldDelta >= addition || oldAdded+oldDelta > oldAllowance || used+usedDelta > budget {
			continue
		}
		room := oldAllowance - oldAdded - oldDelta
		if extra := budget - used - usedDelta; extra < room {
			room = extra
		}
		for _, index := range members {
			plan := plans[index]
			if plan == nil {
				continue
			}
			if ctx.Err() != nil {
				return nil, "deadline"
			}
			chunks, _, failed := r.scoreSourceChunks(ctx, plan.src, plan.head, plan.tail, query)
			if failed != "" {
				return nil, "deadline"
			}
			picked := r.pickSourceChunks(plan.src, chunks, room, 8)
			text, cost := plan.minimal, plan.minimalCost+4
			if len(picked) > 0 {
				grown := contextRecallJoinText(plan.src, plan.head, picked, plan.tail)
				if grownCost := contextEstimateAnyTokens(plan.replace(grown)) + 4; grownCost-(plan.minimalCost+4) <= room {
					text, cost = grown, grownCost
				}
			}
			extra := cost - (plan.minimalCost + 4)
			room -= extra
			usedDelta += extra
			if !baselineMask[index] && index < cutoff {
				oldDelta += extra
			}
			replaced[index] = contextRecallReplaceContent(items[index], plan.replace(text))
			repCost[index] = cost
		}
		for _, index := range members {
			selected[index] = true
		}
		chosen[i] = true
		used += usedDelta
		oldAdded += oldDelta
		recalled = true
	}

	for _, i := range lineFallbackRequired {
		if !chosen[i] {
			return nil, "repeated_query_budget"
		}
	}
	// A required message repaired by shrinking is new content over the legacy
	// baseline even when no separate old unit was brought back.
	repaired := len(replaced) > 0
	if !recalled && !repaired {
		return nil, "no_older_evidence"
	}

	// Spend any remaining space on items already chosen by the legacy trim,
	// newest first. Recall never displaces the protected recent suffix.
	for i := len(turns) - 1; i >= 0; i-- {
		if !chosen[i] && baselineUnit(i) {
			add(i, budget)
		}
	}
	if ctx.Err() != nil {
		return nil, "deadline"
	}
	if enabled, _ := ctx.Value(contextStepToolsGuardKey{}).(bool); enabled {
		if reason := contextRecallElidedReadGuard(ctx, items, baselineMask, selected); reason != "" {
			return nil, reason
		}
	}
	kept = []any{}
	for i, item := range items {
		if !selected[i] {
			continue
		}
		if grown, ok := replaced[i]; ok {
			kept = append(kept, grown)
		} else {
			kept = append(kept, item)
		}
	}
	if contextEstimateItemsTokens(kept) > budget || !contextToolHistorySafeToTrim(kept) || (protocol == "responses" && !contextResponsesItemsSafeToTrim(kept)) {
		return nil, "candidate_invalid"
	}
	actualOldAdded := 0
	for i := range items {
		if baselineMask[i] && i >= cutoff && !selected[i] {
			return nil, "recent_guard_lost"
		}
		if selected[i] && !baselineMask[i] && i < cutoff {
			actualOldAdded += charge(i)
		}
	}
	if actualOldAdded == 0 && len(replaced) == 0 {
		return nil, "no_older_evidence"
	}
	if actualOldAdded > oldAllowance {
		return nil, "old_share_limit"
	}
	return kept, "selected"
}

func contextRecallUnits(items []any) ([]contextRecallTurn, []int) {
	groups := completedContextToolGroupsBefore(items, len(items))
	starts := make(map[int][]int)
	members := make(map[int]bool)
	for _, group := range groups {
		if len(group.indexes) == 0 {
			continue
		}
		indexes := make([]int, 0, len(group.indexes))
		for _, index := range group.indexes {
			// The Anthropic baseline helper can include the initiating user.
			// Ownership is attached below, so the group must start at its call.
			if !contextRecallOrdinaryUser(items[index]) {
				indexes = append(indexes, index)
			}
		}
		if len(indexes) == 0 {
			continue
		}
		sort.Ints(indexes)
		starts[indexes[0]] = indexes
		for _, i := range indexes {
			members[i] = true
		}
	}
	var units []contextRecallTurn
	var anchors, users []int
	owner := -1
	for i, item := range items {
		role := contextItemRole(item)
		if role == "system" || role == "developer" {
			anchors = append(anchors, i)
			continue
		}
		if contextRecallOrdinaryUser(item) {
			owner = i
			users = append(users, i)
			units = append(units, contextRecallTurn{indexes: []int{i}, owner: i})
			continue
		}
		if group, ok := starts[i]; ok {
			if len(units) > 0 && len(units[len(units)-1].indexes) == 1 && units[len(units)-1].indexes[0] == owner && !units[len(units)-1].tool {
				units[len(units)-1].indexes = append(units[len(units)-1].indexes, group...)
				units[len(units)-1].tool = true
			} else {
				unit := contextRecallTurn{owner: owner, tool: true}
				if owner >= 0 {
					unit.indexes = append(unit.indexes, owner)
				}
				unit.indexes = append(unit.indexes, group...)
				units = append(units, unit)
			}
			continue
		}
		if members[i] {
			continue
		}
		if len(units) == 0 {
			units = append(units, contextRecallTurn{owner: -1})
		}
		units[len(units)-1].indexes = append(units[len(units)-1].indexes, i)
	}
	// Keep the current and preceding task statements even when selection
	// removes some completed tool rounds belonging to those tasks.
	for i := len(users) - 1; i >= 0 && i >= len(users)-2; i-- {
		anchors = append(anchors, users[i])
	}
	return units, anchors
}

func contextRecallOrdinaryUser(item any) bool {
	m, _ := item.(map[string]any)
	return contextItemRole(item) == "user" && len(contextAnthropicToolResultIDSet(m)) == 0
}

func contextRecallSupported(value any) bool {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if !contextRecallSupported(item) {
				return false
			}
		}
	case map[string]any:
		if kind, ok := v["type"].(string); ok {
			switch kind {
			case "", "text", "input_text", "output_text", "message", "tool_use", "tool_result", "function", "function_call", "function_call_output", "thinking", "redacted_thinking", "image_url":
			default:
				return false
			}
		}
		for _, key := range []string{"content", "tool_calls", "function"} {
			if !contextRecallSupported(v[key]) {
				return false
			}
		}
	}
	return true
}

func contextRecallText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			b.WriteString(contextRecallText(item))
			b.WriteByte('\n')
		}
		return b.String()
	case map[string]any:
		var b strings.Builder
		for _, key := range []string{"content", "text", "output", "arguments", "tool_calls", "function", "name"} {
			if item, ok := v[key]; ok {
				b.WriteString(contextRecallText(item))
				b.WriteByte('\n')
			}
		}
		if input, ok := v["input"]; ok {
			// Anthropic tool arguments are objects. File identifiers often exist
			// only here, while the corresponding result contains file contents.
			if encoded, err := json.Marshal(input); err == nil {
				b.Write(encoded)
				b.WriteByte('\n')
			}
		}
		return b.String()
	}
	return ""
}

func contextRecallTerms(ctx context.Context, text string) (map[string]int, int, bool) {
	return contextRecallMatchingTerms(ctx, text, nil)
}

// Count all term positions for BM25 normalization, but retain only query terms.
// This bounds index memory by the query rather than by input vocabulary size.
func contextRecallMatchingTerms(ctx context.Context, text string, query map[string]int) (map[string]int, int, bool) {
	terms := make(map[string]int)
	length := 0
	var word strings.Builder
	var previousHan rune
	flush := func() {
		term := strings.Trim(word.String(), "._/-")
		word.Reset()
		if len(term) >= 3 {
			if query == nil || query[term] > 0 {
				terms[term]++
			}
			length++
		}
	}
	checked := 0
	for offset, char := range text {
		if offset-checked >= 4096 {
			checked = offset
			if ctx.Err() != nil || len(terms) > 8192 {
				return nil, 0, false
			}
		}
		if char < 128 && (unicode.IsLetter(char) || unicode.IsDigit(char) || strings.ContainsRune("._/-", char)) {
			word.WriteRune(unicode.ToLower(char))
			previousHan = 0
			continue
		}
		flush()
		if unicode.Is(unicode.Han, char) {
			if previousHan != 0 {
				term := string([]rune{previousHan, char})
				if query == nil || query[term] > 0 {
					terms[term]++
				}
				length++
			}
			previousHan = char
		} else {
			previousHan = 0
		}
	}
	flush()
	return terms, length, ctx.Err() == nil
}

func contextRecallBroadQuery(text string) bool {
	lower := strings.ToLower(text)
	for _, phrase := range []string{"all records", "entire history", "complete conversation", "whole conversation", "whole document", "全文", "所有记录", "所有材料", "全部记录"} {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	return false
}

func contextRecallField(protocol string) string {
	if protocol == "responses" {
		return "input"
	}
	return "messages"
}
