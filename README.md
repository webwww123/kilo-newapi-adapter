# Kilo NewAPI Adapter

Single-base adapter for putting Kilo's no-key gateway behind NewAPI while serving three client protocols:

- OpenAI Chat Completions: `/v1/chat/completions`
- OpenAI Responses: `/v1/responses`
- Anthropic Messages: `/v1/messages`

The adapter rewrites standard paths to Kilo gateway paths. Public model IDs are exposed with
the `kilo/` prefix, and that prefix is stripped before forwarding requests to Kilo.
For selected free models, it also trims oversized request context before forwarding.

## Default Mapping

```text
/v1/chat/completions  -> https://api.kilo.ai/api/gateway/chat/completions
/v1/responses         -> https://api.kilo.ai/api/gateway/responses
/v1/messages          -> https://api.kilo.ai/api/gateway/messages
/v1/models            -> https://api.kilo.ai/api/gateway/models
```

The non-`/v1` variants are also accepted, for relays that strip the prefix.

`/v1/models` returns Kilo's full upstream model list with `kilo/` prefixed IDs.

## Run

```bash
go run ./cmd/kilo-newapi-adapter
```

The service listens on `:18083` by default.

## NewAPI Setup

Use one upstream/base URL:

```text
http://127.0.0.1:18083
```

If your NewAPI build appends `/v1`, use the same base URL. The adapter accepts both `/v1/chat/completions` and `/chat/completions`.

Use model IDs returned by `/v1/models`, for example `kilo/nvidia/nemotron-3-super-120b-a12b:free`.
For compatibility, unprefixed model IDs are still accepted and forwarded unchanged.

`kilo/dots-studio/dots-3-note-preview:free` is a native image-input model. NewAPI channels that should retain that capability must point directly at this adapter instead of an image-to-text gateway, otherwise the original image is replaced before Dots receives it.

## Environment

```text
ADDR=:18083
KILO_BASE_URL=https://api.kilo.ai/api/gateway
MAX_BODY_BYTES=10485760
UPSTREAM_TIMEOUT=180s
REQUEST_LOG_ENABLED=true
REQUEST_LOG_DIR=/var/log/kilo-newapi-adapter
REQUEST_LOG_RETENTION=48h
REQUEST_LOG_BODY_LIMIT=1048576
KILO_AUTO_RELIABILITY_ENABLED=false
KILO_AUTO_REQUEST_COMPAT_ENABLED=false
```

`KILO_API_KEY` is optional. It is not needed for the tested free models. Client `Authorization`, `Cookie`, and upstream override headers are stripped before forwarding to Kilo.

## Public Error Policy

- Upstream HTTP 451 review blocks are returned as HTTP 450 with `触发审查，请重试`.
- Every other client-visible 4xx response is collapsed to HTTP 400 with `400 请求无法处理，请检查请求参数或稍后重试。`.
- Every client-visible 5xx response is collapsed to HTTP 500 with `500 请求无法处理，请检查请求参数或稍后重试。`.
- HTTP 200 error envelopes and SSE error events use the same generic 500 message.
- Original adapter and upstream error details remain in the root-only request log for internal debugging and are never returned to clients.

## Free Model Context Trimming

Context trimming is intentionally limited to manually profiled free models. Paid models and unknown models pass through unchanged.

The trim policy keeps system/developer items, the originating user request, and the newest completed tool turn first. OpenAI `assistant.tool_calls` plus `tool` results, Anthropic `tool_use` plus `tool_result` blocks, and Responses `function_call` plus `function_call_output` items are retained atomically. Oversized newest tool results are truncated while preserving both edges before older conversation turns are considered. If the required current tool structure still cannot fit, the adapter leaves the original request history intact so the upstream can return an explicit context error instead of receiving stale partial history.

Current free-model profiles are maintained in `internal/adapter/context_trim.go`. `stepfun/step-3.7-flash:free` follows the local Yingweida CPA middleware profile: 262144 context, no safety margin, and 220000 max input tokens.
`kilo-auto/free` uses its advertised 256000 context window with a 40000-token safety margin and a 200000-token maximum input budget.
`inclusionai/ling-3.0-flash:free` uses its advertised 262144 context window with a 40000-token safety margin and a 200000-token maximum input budget.
`inclusionai/ling-3.0-tiny:free` uses its advertised 262144 context, 32768 maximum completion tokens, 40000-token safety margin, and 200000-token maximum input budget.
`nvidia/nemotron-3.5-lightning:free` uses its advertised 1000000 context, 65536 maximum completion tokens, a 40000-token safety margin, and a 900000-token maximum input budget.
`tencent/hy3:free` uses its advertised 262144 context and 128000 maximum completion tokens, with a 40000-token safety margin and a 200000-token maximum input budget.
`dots-studio/dots-3-note-preview:free` uses its advertised 512000 context with a 40000-token safety margin, a 400000-token maximum input budget, and a 256000-token safe output cap.
`thinkingmachines/inkling-small:free` and `thinkingmachines/inkling:free` use their advertised 262144 context and 262144 maximum completion metadata, with a 40000-token safety margin and a 200000-token maximum input budget. The shared context guard still reserves at least half the context for input when a client requests an extreme output limit.

HY3, Dots, Inkling Small, and Inkling requests use model-scoped reasoning and output normalization on all three text protocols. Chat Completions forces `reasoning_effort: "minimal"`; Anthropic Messages replaces client thinking controls with the minimum explicit `thinking` budget of 1024; Responses forces `reasoning.effort: "minimal"`. Conflicting reasoning aliases are removed, so client-supplied high, disabled, or oversized settings cannot override the selected minimum. Chat/Messages output aliases are normalized to `max_tokens`, Responses aliases to `max_output_tokens`, and the effective budget is raised to at least 8192 while preserving a higher client value. Live probes showed that fully disabling reasoning made all four model families produce incorrect answers in at least one deterministic task, while low output ceilings could consume the entire allowance before a visible answer. Sibling models remain unchanged.

For HY3 Chat Completions, an OpenAI selected-function `tool_choice` is normalized to the single selected tool plus `tool_choice: "required"`. This preserves the requested function while avoiding Kilo's unreliable selected-function object handling. `auto`, `none`, `required`, malformed selections, and all sibling models are passed through unchanged.

The profiled Ling Flash-free and Ling Tiny-free requests are normalized locally before they reach Kilo. Image, audio, video, and
Embeddings requests are rejected locally with the generic public 400 error. Unsupported reasoning-effort
controls are removed. Chat Completions `json_object` and strict `json_schema` requests are captured before
the upstream cleanup step and emulated in-process for Ling and StepFun 3.7: the adapter validates the completed
object, locally salvages a validated object from hidden reasoning when visible content is empty, repairs with the
same model at most once when salvage is unavailable, and buffers client streaming until validation succeeds.
Hidden reasoning fields are removed from every validated non-stream response and are never emitted in SSE.
Nemotron 3.5 Lightning, Tencent HY3, Dots 3 Note Preview, Inkling Small, Inkling, OpenRouter Free, and Cohere North Mini Code use the same model-scoped local object/strict-Schema validation and single bounded repair path because live probes or production traffic do not reliably enforce `response_format` for those routes. Both `openrouter/free` and its public `kilo/openrouter/free` form resolve to the same exact profile.
For Ling Tiny and Kilo Auto `json_object`, if both validated attempts still return prose or malformed JSON, the adapter emits a bounded local `{ "result": ... }` object with non-zero completion usage instead of returning a successful response with invalid content. Strict `json_schema` never uses this fallback and still fails closed when validation cannot be satisfied. Structured JSON requests for Ling Tiny and Kilo Auto retry transport/429/500/502/503/504 once within a five-second wait budget; review errors are never retried. Dots structured requests do not retry upstream transport failures because production canary evidence showed no recovery benefit under load.
Ordinary Lightning SSE also emits a body-free `kilo_stream_audit` line with event/parse/error counts, visible/tool delta counts, completion tokens, DONE state, and a `suspected_soft_error` flag for correlating HTTP 200 plus zero-completion rows without logging content.
Requests combining tools with `response_format` are rejected locally for these profiles; ordinary tools and
requests without `response_format` stay on the existing path. For the profiled Ling models, explicit `max_tokens`, `max_completion_tokens`,
and `max_output_tokens` values up to 32768 are preserved; for Ling Tiny, positive values below 256 are
raised to 256 so its reasoning phase cannot consume the full allowance before visible output. Larger values are removed so Kilo can
apply its model/provider default instead of returning a generic upstream 400. If an oversized
tool or externally stateful request cannot be trimmed without breaking call/result pairing,
the adapter rejects it locally instead of forwarding stale or partial history; the detailed
reason remains available in the internal request log.

Kilo's provider pool for `stepfun/step-3.7-flash:free` inconsistently rejects OpenAI
`response_format` and does not reliably enforce conflicting strict Schemas. The adapter therefore captures
explicit Chat Completions object/Schema requests, removes the unsupported upstream field, applies a final-format
constraint, validates the completed object locally, and first checks any empty answer's hidden reasoning for a
Schema-valid object. If local salvage is unavailable, StepFun retries once with a bounded repair prompt. Other
models, protocols, ordinary requests, and tool calls without `response_format` are left unchanged.

When `KILO_AUTO_RELIABILITY_ENABLED=true`, ordinary OpenAI Chat requests for `kilo/kilo-auto/free` use a model-scoped visible-output guard. The default is `false`, so deploying a new binary does not activate the prototype without an explicit service configuration change. No-tool first attempts receive a maximum hidden-reasoning budget of 512 tokens unless the client already selected a lower limit or explicitly disabled reasoning; successful tool requests remain unchanged. A non-stream response that is empty, lacks a finish reason, or ends by output length is held locally and repaired once with a 256-token reasoning budget, a 10000-token output floor, and a compact completeness reminder. A second unusable answer becomes the existing generic public 500 instead of leaking reasoning-only output.

When `KILO_AUTO_REQUEST_COMPAT_ENABLED=true`, OpenAI Chat requests for the exact Kilo Auto free model receive conservative dirty-payload repairs before JSON validation, reliability attempts, and upstream forwarding. The adapter wraps recoverable flat function tools, repairs harmless empty Schema variants, serializes structured tool-call arguments, drops unnamed tools only when tool use is optional, converts legacy `role:function` results, normalizes object/legacy content blocks, removes unsupported audio/video blocks while preserving text, removes trailing empty user placeholders only after a completed tool turn, and promotes recognizable flattened `json_schema` roots. Standard text/image/file blocks remain unchanged. Forced or required unnamed tools, genuinely empty user requests, and non-empty malformed Schemas still fail closed. Non-empty argument strings are left unchanged rather than guessed, so their final acceptance remains upstream-dependent. Structural logs contain only repair counts and sanitized error classes; request bodies, tool names/arguments, headers, and credentials are never logged by this feature. The default is `false`.

Chat requests for Step 3.7 Flash, Kilo Auto free, and Inkling free also normalize
completed historical function calls at the forwarding boundary. Missing/null
arguments become an explicit empty string, and object arguments become their
JSON string representation with numeric precision retained. This fixes the
observed Kilo string-type rejection without inventing tool input values. It does
not depend on the Auto feature flag. Calls must have a unique matching later
tool result; pending or ambiguous calls, malformed non-empty argument strings,
other models, and other protocols remain unchanged. Only normalization counts
are logged.

For ordinary Auto streaming, the adapter holds at most the first 1 MiB until visible content or a tool-call delta appears. It then commits immediately and never retries after meaningful data is emitted. If the stream ends or the prelude exceeds the bound before meaningful data, the first attempt is discarded and repaired once; a second empty/overflowing attempt becomes the generic public 500. Policy/error events are not retried. Structured JSON streaming keeps using the separate validated JSON compatibility path described above.

Before public error normalization, Auto Chat retries pre-header transport failures and raw upstream HTTP 429/502/503/504 only. The shared request budget allows at most two raw retries with 1-second and 3-second backoff plus jitter, honors `Retry-After` within a 15-second total wait budget, and is shared with the semantic repair attempt. HTTP 400/413/450/451 and raw 500 are never retried. The combined worst case is four upstream calls for one client request; ordinary successful requests still use one.

Body-free structural journal records use `kilo_auto_reliability` for semantic decisions and `kilo_upstream_retry scope=auto` for raw transient retries. They include only stream/attempt/retry/status/timing fields and never contain prompts, candidates, reasoning text, tool arguments, headers, or credentials.

Request debug logs are written as JSONL files named `requests-YYYY-MM-DD.jsonl`.
Each entry records request metadata, the model field, truncated request and response bodies, upstream status, and timing.
The log directory is created with `0700`, files are created with `0600`, and files older than `REQUEST_LOG_RETENTION` are removed at startup.

## Probes

Chat Completions:

```bash
curl -sS http://127.0.0.1:18083/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","messages":[{"role":"user","content":"Reply with exactly OK and no other text."}],"max_tokens":64}' | jq
```

Responses:

```bash
curl -sS http://127.0.0.1:18083/v1/responses \
  -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","input":"Reply with exactly OK and no other text.","max_output_tokens":64}' | jq
```

Messages:

```bash
curl -sS http://127.0.0.1:18083/v1/messages \
  -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","messages":[{"role":"user","content":"Reply with exactly OK and no other text."}],"max_tokens":64}' | jq
```

Count tokens fallback:

```bash
curl -sS http://127.0.0.1:18083/v1/messages/count_tokens \
  -H 'Content-Type: application/json' \
  -d '{"model":"nvidia/nemotron-3-super-120b-a12b:free","messages":[{"role":"user","content":"hello"}]}' | jq
```

## Docker

```bash
docker build -t kilo-newapi-adapter .
docker run --rm -p 18083:18083 kilo-newapi-adapter
```

## Systemd

```bash
go build -o /usr/local/bin/kilo-newapi-adapter ./cmd/kilo-newapi-adapter
sudo cp deploy/kilo-newapi-adapter.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now kilo-newapi-adapter
```
