# Optional bounded context recall

The adapter can recover complete, relevant older turns before its existing
context trim removes them. The existing model profile still decides the input
budget. System/developer policies, the latest task statements and the latest
two complete interactions stay intact;
message order, roles, tool call/result IDs and original text are preserved.
This is lexical retrieval, not a larger model context window. Whole-history
analysis, paraphrases without shared terms and oversized individual messages can
still lose information through the original trimming path.

Tool-call/result groups form atomic selection units, carrying their originating
user task for interpretation. Many tool rounds within one user task are not
treated as one giant indivisible block. Tool arguments are searchable because
file paths often appear only in the call, not in the returned file contents.

The feature is off unless `CONTEXT_RECALL_CONFIG` names a local control file.
The file contains `{"mode":"shadow","percent":5}` for observation or
`{"mode":"apply","percent":5}` for applying selected results. Percent is an
integer from 0 to 100; 0, a missing file or invalid configuration disables it.
Update the small root-owned file atomically. The process refreshes it at most
once a second, so disabling or changing percentage needs no restart.

Sampling does not duplicate model requests. Shadow computes a candidate while
forwarding the exact legacy result. The control cohort always uses legacy
trimming. Only requests that exceed the existing input budget enter selection;
unknown profiles and previously excluded stateful/unsafe structures retain
their prior behavior. No channel routing, pricing, retry, model mapping or
protocol conversion rule changes.

Selection has two non-queueing slots, a 12 MiB input cap, 4096 item cap and a
cooperative 250 ms deadline. Busy, cancelled, oversized, unsupported, broad
query, weak lexical evidence, invalid candidate and internal failure paths
immediately use the original trimmer on untouched content. A selected candidate
is rechecked with the original budget and tool-history validator. Time already
spent in selection remains a cost; fail-open does not mean zero extra latency.

`context_recall` structural records contain the cohort, selection reason,
applied flag, input byte/item count, budget and timing. Optional request
correlation is SHA-256 truncated to 128 bits, using the existing request ID;
the ID itself, messages, tool arguments, credentials and upstream URL are never
included. HTTP status is not an answer-quality or completed-stream metric.
Use gateway logical-request records for final outcome and cumulative wait.

Before applying, use CI-built artifacts and record the previous service binary
and configuration. Observe shadow resource cost, then increase the configured
percentage only while endpoint health, process memory and gateway final outcome
remain acceptable. Set percent to 0 immediately on regression. A Kilo adapter
rollout covers only requests that actually reach this trimmer; it does not
cover the separate custom-auto gateway or other middleware families.

Tests keep request-log directories inside temporary directories. The initial
local HTTP test omitted that override and triggered the existing constructor's
48-hour cleanup of the local default log directory. The number removed is not
known; the test was fixed before deployment. No model request or production
service restart is performed by integration tests.
