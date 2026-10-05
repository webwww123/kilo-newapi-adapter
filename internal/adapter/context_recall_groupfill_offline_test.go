//go:build recall_offline

package adapter

// Offline acceptance replay for the groupfill recall fix. Reads the recorded
// production corpus straight from the local analysis directories; the corpus
// itself never enters the repository, so on machines without it (CI runners)
// every sample skips cleanly. No network access, no upstream calls; the
// selection deadline is widened so replay results do not reproduce online
// timeouts.
//
// For every recorded request the buggy build answered with
// reason=selected the fix must keep all of:
//  1. the positions inside the input's last twenty items that the legacy
//     trim kept, plus every item the legacy trim kept after the latest
//     ordinary user message;
//  2. a new/old output byte ratio of at least 0.95;
//  3. for samples that originally had cut_terms>0, a final_hit no lower
//     than 90% of the recorded value.
//
// This source revision predates the metric pipeline, so the test recomputes
// final_hit here with the same definition the production metric uses: of the
// effective question terms the legacy trim cut beyond the question span, how
// many the new output shows more often than the question span alone.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

type recallTruthRow struct {
	ID           string `json:"id"`
	UpstreamPath string `json:"upstream_path"`
	ProfileModel string `json:"profile_model"`
	RecallReason string `json:"recall_reason"`
	CutTerms     int    `json:"cut_terms"`
	FinalHit     int    `json:"final_hit"`
	Skip         string `json:"skip"`
}

var recallTruthArmDirs = []string{
	"/root/devin-work/recall-truth/replay",
	"/root/devin-work/recall-drop/scope/replay-stealth",
	"/root/devin-work/recall-drop/scope/replay-apodex",
}

func recallTruthProfile(model string) (contextTrimProfile, bool) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(model), publicModelPrefix)
	if p, ok := freeContextTrimProfiles[trimmed]; ok {
		return p, true
	}
	// The kilo fleet's dedicated apodex adapter carries this profile; the
	// shared table does not. Replaying apodex traffic uses the same numbers
	// so the recall engine sees the real online budget.
	if trimmed == "apodex/apodex-1.1-mini:free" {
		return contextTrimProfile{ContextLimit: 262144, SafetyMargin: 40000, MaxInputTokens: 200000, DefaultOutputTokens: 8192, MaxOutputTokens: 235929}, true
	}
	return contextTrimProfile{}, false
}

// recallSeqMatch maps each kept item to its source position, scanning forward
// in order. A fragment-repaired copy (same keys and values except content)
// maps to -(src+1). Returns nil when the kept sequence is not a subsequence.
func recallSeqMatch(kept, src []any) []int {
	sameExceptContent := func(a, b map[string]any) bool {
		for key, va := range a {
			if key == "content" {
				continue
			}
			if !reflect.DeepEqual(va, b[key]) {
				return false
			}
		}
		for key, vb := range b {
			if key == "content" {
				continue
			}
			if !reflect.DeepEqual(vb, a[key]) {
				return false
			}
		}
		return true
	}
	idxs := make([]int, 0, len(kept))
	at := 0
	for _, k := range kept {
		found := -1
		for at < len(src) {
			if reflect.DeepEqual(src[at], k) {
				found = at
				break
			}
			at++
		}
		if found < 0 {
			for j, s := range src {
				sm, sok := s.(map[string]any)
				km, kok := k.(map[string]any)
				if !sok || !kok {
					continue
				}
				if sameExceptContent(sm, km) {
					found = j
					idxs = append(idxs, -(j + 1))
					break
				}
			}
			if found < 0 {
				return nil
			}
		} else {
			idxs = append(idxs, found)
		}
		at = found + 1
	}
	return idxs
}

func recallKeptPositions(kept, src []any) map[int]bool {
	idxs := recallSeqMatch(kept, src)
	out := make(map[int]bool, len(idxs))
	for _, i := range idxs {
		if i < 0 {
			out[-(i + 1)] = true
		} else {
			out[i] = true
		}
	}
	return out
}

func recallLatestUserIndex(items []any) int {
	for i := len(items) - 1; i >= 0; i-- {
		if contextRecallOrdinaryUser(items[i]) {
			return i
		}
	}
	return len(items) - 1
}

func recallTruthItems(t *testing.T, path, field string) []any {
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	items, _ := body[field].([]any)
	return items
}

// recallTestFinalHit replays the production metric's final_hit definition:
// cut terms are the effective question terms (df*3 < turns, capped at 64)
// whose occurrences beyond the question span the baseline dropped in full;
// a cut term is hit when the new output shows it more often than the question
// span alone. Tool-owner items never contribute terms, matching the
// selector's own accounting. Returns hit=-1 when the question has no
// evaluable terms, mirroring the metric's unset state; cut is the recomputed
// cut-term count for cross-checking the recorded row.
func recallTestFinalHit(ctx context.Context, srcItems, newItems, baselineItems []any) (hit, cut int) {
	// The query is built exactly like the selector: the final paragraph of
	// the latest ordinary user message (plus the head of an oversized one),
	// extended to the newest six user turns for continuation questions.
	query := make(map[string]int)
	questionSpan := ""
	userTurns := 0
	queryTurnLimit := 1
	for i := len(srcItems) - 1; i >= 0 && userTurns < queryTurnLimit; i-- {
		if !contextRecallOrdinaryUser(srcItems[i]) {
			continue
		}
		userTurns++
		full := contextRecallText(srcItems[i])
		text := full
		if userTurns == 1 {
			question := contextRecallQuestionText(full)
			questionSpan = question
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
			return -1, 0
		}
		for term := range terms {
			query[term] = 1
		}
	}
	if len(query) == 0 || questionSpan == "" {
		return -1, 0
	}
	turns, _ := contextRecallUnits(srcItems)
	if len(turns) == 0 {
		return -1, 0
	}
	// Exact-equality subsequence match, identical to the selector's
	// baseline mask: modified copies count as dropped.
	baselineMask := make([]bool, len(srcItems))
	baselineAt := 0
	for i, item := range srcItems {
		if baselineAt < len(baselineItems) && reflect.DeepEqual(item, baselineItems[baselineAt]) {
			baselineMask[i] = true
			baselineAt++
		}
	}
	owner := map[int]bool{}
	turnTerms := make([]map[string]int, len(turns))
	df := make(map[string]int)
	for i := range turns {
		var text strings.Builder
		for _, index := range turns[i].indexes {
			if turns[i].tool && index == turns[i].owner {
				continue
			}
			text.WriteString(contextRecallText(srcItems[index]))
			text.WriteByte('\n')
		}
		terms, _, ok := contextRecallMatchingTerms(ctx, text.String(), query)
		if !ok {
			return -1, 0
		}
		turnTerms[i] = terms
		for term := range terms {
			df[term]++
		}
		if turns[i].tool && turns[i].owner >= 0 {
			owner[turns[i].owner] = true
		}
	}
	eff := make([]string, 0, len(query))
	for term := range query {
		if df[term]*3 < len(turns) {
			eff = append(eff, term)
		}
	}
	sort.Strings(eff)
	if len(eff) > 64 {
		eff = eff[:64]
	}
	if len(eff) == 0 {
		return -1, 0
	}
	evalQuery := make(map[string]int, len(eff))
	for _, term := range eff {
		evalQuery[term] = 1
	}
	occQ, _, ok2 := contextRecallMatchingTerms(ctx, questionSpan, query)
	if !ok2 {
		return -1, 0
	}
	occ := make(map[string]int, len(query))
	cutOcc := make(map[string]int, len(eff))
	for i := range turns {
		for term, count := range turnTerms[i] {
			occ[term] += count
		}
	}
	perItem := make(map[int]map[string]int)
	for i, item := range srcItems {
		role := contextItemRole(item)
		if role != "system" && role != "developer" && !owner[i] {
			continue
		}
		terms, _, ok := contextRecallMatchingTerms(ctx, contextRecallText(item), query)
		if !ok {
			return -1, 0
		}
		perItem[i] = terms
		for term, count := range terms {
			occ[term] += count
			if !baselineMask[i] {
				cutOcc[term] += count
			}
		}
	}
	for i := range turns {
		allCut, anyCut := true, false
		for _, index := range turns[i].indexes {
			if turns[i].tool && index == turns[i].owner {
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
			for term, count := range turnTerms[i] {
				cutOcc[term] += count
			}
		case anyCut:
			for _, index := range turns[i].indexes {
				if (turns[i].tool && index == turns[i].owner) || baselineMask[index] {
					continue
				}
				terms, _, ok := contextRecallMatchingTerms(ctx, contextRecallText(srcItems[index]), evalQuery)
				if !ok {
					return -1, 0
				}
				for term, count := range terms {
					cutOcc[term] += count
				}
			}
		}
	}
	var cutSet []string
	for _, term := range eff {
		if occ[term] <= occQ[term] {
			continue
		}
		if occ[term]-cutOcc[term] > occQ[term] {
			continue
		}
		cutSet = append(cutSet, term)
	}
	if len(cutSet) == 0 {
		return 0, 0
	}
	cutQuery := make(map[string]int, len(cutSet))
	for _, term := range cutSet {
		cutQuery[term] = 1
	}
	// Map the new output back to source positions; a modified copy keeps its
	// position but contributes its rewritten text to the count.
	idxs := recallSeqMatch(newItems, srcItems)
	if idxs == nil {
		return -1, len(cutSet)
	}
	newSet := make(map[int]bool, len(idxs))
	newItem := make(map[int]any, len(idxs))
	for k, i := range idxs {
		pos := i
		if pos < 0 {
			pos = -(pos + 1)
			newSet[pos] = true
			newItem[pos] = newItems[k]
			continue
		}
		newSet[pos] = true
	}
	fin := make(map[string]int, len(cutSet))
	for i := range turns {
		allKept, anyKept, changed := true, false, false
		for _, index := range turns[i].indexes {
			if turns[i].tool && index == turns[i].owner {
				continue
			}
			if !newSet[index] {
				allKept = false
				continue
			}
			anyKept = true
			if _, ok := newItem[index]; ok {
				changed = true
			}
		}
		switch {
		case allKept && !changed:
			for term, count := range turnTerms[i] {
				fin[term] += count
			}
		case anyKept:
			for _, index := range turns[i].indexes {
				if (turns[i].tool && index == turns[i].owner) || !newSet[index] {
					continue
				}
				item := srcItems[index]
				if rep, ok := newItem[index]; ok {
					item = rep
				}
				terms, _, ok := contextRecallMatchingTerms(ctx, contextRecallText(item), cutQuery)
				if !ok {
					return -1, len(cutSet)
				}
				for term, count := range terms {
					fin[term] += count
				}
			}
		}
	}
	for index, terms := range perItem {
		if !newSet[index] {
			continue
		}
		if rep, ok := newItem[index]; ok {
			reTerms, _, ok := contextRecallMatchingTerms(ctx, contextRecallText(rep), cutQuery)
			if !ok {
				return -1, len(cutSet)
			}
			terms = reTerms
		}
		for term, count := range terms {
			fin[term] += count
		}
	}
	for _, term := range cutSet {
		if fin[term] > occQ[term] {
			hit++
		}
	}
	return hit, len(cutSet)
}

func TestContextRecallGroupfillAcceptance(t *testing.T) {
	type sample struct {
		row recallTruthRow
		dir string
	}
	var samples []sample
	for _, arm := range recallTruthArmDirs {
		index := filepath.Join(arm, "replay.jsonl")
		raw, err := os.ReadFile(index)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var row recallTruthRow
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				continue
			}
			if row.Skip != "" || row.RecallReason != "selected" {
				continue
			}
			samples = append(samples, sample{row: row, dir: filepath.Join(arm, "records", row.ID)})
		}
	}
	if len(samples) == 0 {
		t.Skip("recall-truth replay corpus not present on this machine")
	}

	passTail, passRatio, passHit := 0, 0, 0
	hitSamples := 0
	var failures []string
	for _, s := range samples {
		if _, err := os.Stat(filepath.Join(s.dir, "input.json")); err != nil {
			t.Fatalf("sample %s: record dir missing: %v", s.row.ID, err)
		}
		profile, ok := recallTruthProfile(s.row.ProfileModel)
		if !ok {
			t.Fatalf("sample %s: no trim profile for %s", s.row.ID, s.row.ProfileModel)
		}
		field := "messages"
		if strings.Contains(s.row.UpstreamPath, "/responses") {
			t.Fatalf("sample %s: responses-path replay not supported by this test", s.row.ID)
		}
		srcItems := recallTruthItems(t, filepath.Join(s.dir, "input.json"), field)
		oldItems := recallTruthItems(t, filepath.Join(s.dir, "old.json"), field)
		oldBytes, err := os.ReadFile(filepath.Join(s.dir, "old.json"))
		if err != nil {
			t.Fatalf("sample %s: %v", s.row.ID, err)
		}

		inputRaw, err := os.ReadFile(filepath.Join(s.dir, "input.json"))
		if err != nil {
			t.Fatalf("sample %s: %v", s.row.ID, err)
		}
		var payload map[string]any
		if err := json.Unmarshal(inputRaw, &payload); err != nil {
			t.Fatalf("sample %s: %v", s.row.ID, err)
		}
		engine := newContextRecallEngine()
		engine.timeout = 10 * time.Minute
		var lastEvent contextRecallEvent
		control := &contextRecallControl{engine: engine, emit: func(e contextRecallEvent) { lastEvent = e }}
		q := &contextRecallRequest{
			control: control, ctx: context.Background(),
			chosen: true, fragmentChosen: true, repeatedChosen: true,
			event: contextRecallEvent{Version: 1, Mode: "apply", Percent: 100, FragmentPercent: 100, RepeatedPercent: 100,
				BaselineTailKept: -1, BudgetUsePct: -1},
		}
		q.event.RepeatedEnabled = true
		var stats contextTrimStats
		trimMessagePayload(payload, field, profile, false, &stats, q)
		q.finish(200, 0)
		newItems, _ := payload[field].([]any)
		newRaw, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("sample %s: marshal output: %v", s.row.ID, err)
		}

		oldSet := recallKeptPositions(oldItems, srcItems)
		newSet := recallKeptPositions(newItems, srcItems)
		n := len(srcItems)
		latestUser := recallLatestUserIndex(srcItems)
		lost20 := 0
		for p := n - 20; p < n; p++ {
			if p >= 0 && oldSet[p] && !newSet[p] {
				lost20++
			}
		}
		curTurnLost := 0
		for p := latestUser + 1; p < n; p++ {
			if oldSet[p] && !newSet[p] {
				curTurnLost++
			}
		}
		ratio := float64(len(newRaw)) / float64(len(oldBytes))
		tailOK := lost20 == 0 && curTurnLost == 0
		// Any bounded fallback must return the legacy trim byte-for-byte.
		if lastEvent.Reason != "selected" && !bytes.Equal(newRaw, oldBytes) {
			tailOK = false
		}
		ratioOK := ratio >= 0.95
		hitOK := true
		finalHit, cutNow := -1, -1
		if s.row.CutTerms > 0 {
			hitSamples++
			if lastEvent.Reason == "selected" {
				finalHit, cutNow = recallTestFinalHit(context.Background(), srcItems, newItems, oldItems)
				hitOK = finalHit >= 0 && float64(finalHit) >= 0.9*float64(s.row.FinalHit)
			} else {
				// A bounded fallback is sanctioned only when it returns the
				// legacy trim verbatim: the buggy hits it replaces were bought
				// by dropping the recent tail, so there is no applied output
				// to compare. Anything else would be a silent content change.
				if !bytes.Equal(newRaw, oldBytes) {
					hitOK = false
				}
			}
		}
		if tailOK {
			passTail++
		}
		if ratioOK {
			passRatio++
		}
		if hitOK {
			passHit++
		}
		t.Logf("%s reason=%-24s lost20=%d cur_turn_lost=%d ratio=%.3f fh=%d(was %d) cut=%d(was %d) tail_kept=%d budget_use=%d",
			s.row.ID[:20], lastEvent.Reason, lost20, curTurnLost, ratio,
			finalHit, s.row.FinalHit, cutNow, s.row.CutTerms, lastEvent.BaselineTailKept, lastEvent.BudgetUsePct)
		if !tailOK || !ratioOK || !hitOK {
			failures = append(failures, s.row.ID)
		}
	}
	t.Logf("tail-kept %d/%d, byte-ratio>=0.95 %d/%d, final_hit>=90%%-or-sanctioned-fallback %d/%d (of %d with cut_terms>0)",
		passTail, len(samples), passRatio, len(samples), passHit, hitSamples, hitSamples)
	if len(failures) > 0 {
		t.Fatalf("acceptance failed for %d/%d samples: %s", len(failures), len(samples), strings.Join(failures, ","))
	}
}
