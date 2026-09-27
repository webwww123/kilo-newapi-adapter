# Offline context recall experiment

This experiment compares the deployed context trimmer with a bounded lexical
history selector. It does not call a model, collect customer prompts, change
production routing, or prove answer quality. Experimental Go files have both
the `recall_offline` build tag and the `_test.go` suffix; normal service builds
cannot include the experiment. No existing production file is modified.

## Fixed baseline and evaluation plan

- Baseline: the unmodified `trimMessagePayload` / `trimResponsePayload` at
  source revision `033517b6fed09378252968e6569674219f41cc8f`, checked against the
  live binary before the experiment.
- Use the baseline estimator in both arms. Input targets of 500,000 and
  1,000,000 are estimates, not a provider tokenizer's exact token counts.
- Test 500k input into 128k usable input, 1m into 128k, and 1m into 256k.
  Reserve the same output, fixed-field and tool-definition budget in both arms.
- Repeat the same semantic scenarios for Chat, Messages and Responses.
- Synthetic evidence is placed early, in the middle and near the end. Expected
  facts exist only in the evaluator, never in the selector's input arguments.
- Positive and adverse scenarios are fixed before implementing the selector:
  named old file, early Chinese constraint, completed historical tool result,
  continue after a substantive task, corrected earlier decision, paraphrase
  without lexical overlap, repeated-keyword distractors, aggregate evidence
  across the whole history, and a recent-information control.
- Separate guard cases cover unchanged small input, external Responses state,
  unknown items, broken tool pairs, images, oversized current message/result,
  resource saturation, cancellation, timeout and input-size limits.

Report retention of the required verbatim evidence, including losses and ties.
This metric is not a model's answer correctness, understanding, or customer
success rate. Fixtures with all relevant evidence larger than the output budget
must be reported as limitations, not removed from the denominator.

## Candidate constraints

Keep system/developer items, the current task and complete tool turns. Preserve
roles, text, identifiers and ordering. Prefer recent turns for most of the budget
and use the remainder for lexical recall from earlier complete turns. Do not
invent a summary or split a tool payload. Unsupported or oversized mandatory
structures use the exact old trimmer from untouched input. Bound input size,
item count, execution time and simultaneous recall jobs; never wait for a slot.

## Local execution only

Run the tagged, narrowly selected tests with `GOPROXY=off GOSUMDB=off
GOTOOLCHAIN=local GOMAXPROCS=1`, using a low scheduling priority. The report path
is supplied through `RECALL_OFFLINE_REPORT`. Performance is measured in separate
baseline/candidate processes to keep peak RSS comparisons meaningful.

Results and limitations will be appended after the fixed comparison. Production
integration, model answer comparisons and rollout are separate decisions.

## Observed results, 2026-09-27

The local comparison completed 81 combinations: nine constructed scenario
families, three protocol representations, and three input/budget combinations.
These are not 81 independent customer tasks. All 81 candidate selections passed
the unchanged-field, input-budget, ordered-original-item, current-task and
tool-pair checks. All 32 fallback subcases passed. Six simultaneous selection
jobs admitted two and immediately rejected four to the old path; slots were
released. The isolated concurrency test also passed the Go race detector.

| Scenario family | All required evidence retained, baseline | Candidate |
|---|---:|---:|
| Named old file | 0/9 | 9/9 |
| Early Chinese constraint | 0/9 | 9/9 |
| Historical complete tool result | 9/9 | 9/9 |
| Continue a prior substantive task | 6/9 | 9/9 |
| Corrected previous decision | 0/9 | 9/9 |
| Cross-language semantic paraphrase | 0/9 | 0/9 |
| Repeated-keyword distractors | 0/9 | 3/9 |
| Whole-history aggregation | 0/9 | 0/9 |
| Recent-information control | 6/9 | 9/9 |

Thirty-nine combinations retained more of the evaluated facts; 42 retained the
same count. **This is not a no-regression result.** Whole-history cases retained
177 versus 180 out of 864 required facts, but 112 previously retained facts were
replaced by different ones. Neither method can satisfy those aggregation tasks.
The experiment has no model answers, answer-accuracy score, or customer metric.

Paired synthetic cases added a median 29.2 ms and a maximum observed 50.8 ms.
Separate serial processes on the approximately 1m-estimated-input, 128k-budget
case took about 12.2 ms baseline versus 44.4 ms candidate (six measured runs each,
after one warm-up). These include parsing and serialization. The corresponding
process address-space high-water RSS was approximately 20.9 versus 21.9 MiB.
Those are complete test-process figures, not incremental production RAM. The
candidate allocated about 10.6 MiB cumulatively per request versus 2.7 MiB for
the baseline; allocations are not peak live memory. Keep the earlier production
resource reserve until real integration and concurrent workloads are measured.

Raw Linux rusage high-water readings were about 149 MiB for both processes,
exceeding the current address space's VmHWM. The report retains both and uses
VmHWM/VmRSS for the current address space; the larger rusage reading may reflect
a pre-exec high-water mark. No virtual-address-space size is used.

See `context-recall-offline-results.json` for machine-readable aggregates. The
original production trimmer retains SHA-256
`da0243984cfafa898b68aa2470a280fed96cfd54e1eb57ecc1a9fb31f0f036e9`.
Normal-build `go list` excludes all three experimental Go files; unchanged
baseline context tests also pass. The initial compilation missed one fixture
switch brace; this was corrected before any results. Later changes sorted query
terms for deterministic floating-point accumulation and improved memory
measurement; no fixture facts were tuned to improve the results.

Recommendation: the lexical prototype warrants a small model-answer comparison
for explicit references to old facts. It does **not** justify a general rollout.
Semantic paraphrases, full-document synthesis and single giant payloads remain
outside the demonstrated benefit. Resource fallback preserves the old trimming
result, but cooperative deadline checks and shared resources do not establish
zero production availability risk.
