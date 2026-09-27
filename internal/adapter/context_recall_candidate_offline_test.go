//go:build recall_offline

package adapter

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Test-only prototype: no production request path can reference this code.
type recallExperiment struct {
	slots    chan struct{}
	maxBytes int
	maxItems int
	timeout  time.Duration
}

func newRecallExperiment() *recallExperiment {
	return &recallExperiment{slots: make(chan struct{}, 2), maxBytes: 12 << 20, maxItems: 4096, timeout: 250 * time.Millisecond}
}

type recallTurn struct {
	indexes []int
	cost    int
	terms   map[string]int
	length  int
	score   float64
}

func (r *recallExperiment) choose(ctx context.Context, payload map[string]any, protocol string, rawBytes, budget int) ([]any, string) {
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
	field := recallField(protocol)
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
	if !contextToolHistorySafeToTrim(items) || !recallSupported(items) {
		return nil, "unsupported_structure"
	}
	if budget < 512 {
		return nil, "fixed_fields_overflow"
	}
	if contextEstimateItemsTokens(items) <= budget {
		return nil, "not_needed"
	}
	turns := []recallTurn{}
	anchors := []int{}
	for i, item := range items {
		role := contextItemRole(item)
		if role == "system" || role == "developer" {
			anchors = append(anchors, i)
			continue
		}
		if len(turns) == 0 || recallOrdinaryUser(item) {
			turns = append(turns, recallTurn{})
		}
		g := &turns[len(turns)-1]
		g.indexes = append(g.indexes, i)
		g.cost += contextEstimateAnyTokens(item) + 4
	}
	if len(turns) == 0 {
		return nil, "no_turns"
	}
	selected := make([]bool, len(items))
	chosen := make([]bool, len(turns))
	used := 0
	for _, i := range anchors {
		selected[i] = true
		used += contextEstimateAnyTokens(items[i]) + 4
	}
	add := func(i, limit int) bool {
		if chosen[i] || used+turns[i].cost > limit {
			return false
		}
		for _, index := range turns[i].indexes {
			selected[index] = true
		}
		chosen[i] = true
		used += turns[i].cost
		return true
	}
	// Keep the current complete turn and preceding complete interaction intact.
	for i := len(turns) - 1; i >= 0 && i >= len(turns)-2; i-- {
		if !add(i, budget) {
			return nil, "mandatory_turn_too_large"
		}
	}
	if used > budget {
		return nil, "mandatory_turn_too_large"
	}
	for i := len(turns) - 1; i >= 0; i-- {
		add(i, budget*3/4)
	}
	query := make(map[string]int)
	userTurns := 0
	for i := len(items) - 1; i >= 0 && userTurns < 6; i-- {
		if !recallOrdinaryUser(items[i]) {
			continue
		}
		userTurns++
		text := recallText(items[i])
		if len(text) > 8192 {
			text = text[:4096] + " " + text[len(text)-4096:]
		}
		terms, _, ok := recallTerms(ctx, text)
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
	queryTerms := make([]string, 0, len(query))
	for term := range query {
		queryTerms = append(queryTerms, term)
	}
	sort.Strings(queryTerms)
	df := make(map[string]int)
	totalLength := 0
	for i := range turns {
		if ctx.Err() != nil {
			return nil, "deadline"
		}
		var text strings.Builder
		for _, index := range turns[i].indexes {
			text.WriteString(recallText(items[index]))
			text.WriteByte('\n')
		}
		terms, length, ok := recallTerms(ctx, text.String())
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
	for i := range turns {
		if ctx.Err() != nil {
			return nil, "deadline"
		}
		if chosen[i] {
			continue
		}
		for _, term := range queryTerms {
			tf := float64(turns[i].terms[term])
			if tf == 0 {
				continue
			}
			idf := math.Log(1 + (float64(len(turns)-df[term])+0.5)/(float64(df[term])+0.5))
			turns[i].score += idf * tf * 2.2 / (tf + 1.2*(0.25+0.75*float64(turns[i].length)/avg))
		}
		if turns[i].score > 0 {
			ranked = append(ranked, i)
		}
	}
	sort.SliceStable(ranked, func(a, b int) bool {
		i, j := ranked[a], ranked[b]
		if turns[i].score == turns[j].score {
			return i > j
		}
		return turns[i].score > turns[j].score
	})
	for _, i := range ranked {
		add(i, budget)
	}
	for i := len(turns) - 1; i >= 0; i-- {
		add(i, budget)
	}
	if ctx.Err() != nil {
		return nil, "deadline"
	}
	kept := []any{}
	for i, item := range items {
		if selected[i] {
			kept = append(kept, item)
		}
	}
	if contextEstimateItemsTokens(kept) > budget || !contextToolHistorySafeToTrim(kept) || (protocol == "responses" && !contextResponsesItemsSafeToTrim(kept)) {
		return nil, "candidate_invalid"
	}
	return kept, "selected"
}

func recallOrdinaryUser(item any) bool {
	m, _ := item.(map[string]any)
	return contextItemRole(item) == "user" && len(contextAnthropicToolResultIDSet(m)) == 0
}

func recallSupported(value any) bool {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if !recallSupported(item) {
				return false
			}
		}
	case map[string]any:
		if kind, ok := v["type"].(string); ok {
			switch kind {
			case "", "text", "input_text", "output_text", "message", "tool_use", "tool_result", "function", "function_call", "function_call_output":
			default:
				return false
			}
		}
		for _, key := range []string{"content", "tool_calls", "function"} {
			if !recallSupported(v[key]) {
				return false
			}
		}
	}
	return true
}

func recallText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			b.WriteString(recallText(item))
			b.WriteByte('\n')
		}
		return b.String()
	case map[string]any:
		var b strings.Builder
		for _, key := range []string{"content", "text", "output", "arguments"} {
			if item, ok := v[key]; ok {
				b.WriteString(recallText(item))
				b.WriteByte('\n')
			}
		}
		return b.String()
	}
	return ""
}

func recallTerms(ctx context.Context, text string) (map[string]int, int, bool) {
	terms := make(map[string]int)
	length := 0
	var word strings.Builder
	var previousHan rune
	flush := func() {
		term := strings.Trim(word.String(), "._/-")
		word.Reset()
		if len(term) >= 3 {
			terms[term]++
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
				terms[string([]rune{previousHan, char})]++
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

func runRecallExperiment(ctx context.Context, raw []byte, protocol string, profile contextTrimProfile, candidate *recallExperiment) ([]byte, string, error) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, "decode_error", err
	}
	payload, ok := decoded.(map[string]any)
	if !ok {
		return raw, "non_object", nil
	}
	var stats contextTrimStats
	reason := "baseline"
	if candidate != nil {
		responses := protocol == "responses"
		budget := contextInputBudget(payload, profile, responses, &stats)
		if responses {
			budget -= contextEstimateAnyTokens(payload["instructions"])
		} else {
			budget -= contextEstimateAnyTokens(payload["system"])
		}
		kept, why := candidate.choose(ctx, payload, protocol, len(raw), budget)
		reason = why
		if why == "selected" {
			payload[recallField(protocol)] = kept
			out, err := json.Marshal(payload)
			return out, reason, err
		}
	}
	// Selection is read-only. Fallback always starts from the original message
	// contents and uses the exact deployed trimmer, including its safety guards.
	if protocol == "responses" {
		trimResponsePayload(payload, profile, &stats)
	} else {
		trimMessagePayload(payload, "messages", profile, false, &stats)
	}
	if !stats.changed() {
		return raw, reason, nil
	}
	out, err := json.Marshal(payload)
	return out, reason, err
}
