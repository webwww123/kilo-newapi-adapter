# Kilo Auto Reliability Profile

## Scope

- Public model: `kilo/kilo-auto/free` (prefixed and internal unprefixed forms).
- Endpoint: OpenAI Chat Completions only.
- Ordinary requests only for semantic repair; requests with `response_format` remain owned by the existing structured JSON compatibility handler.
- Anthropic Messages and OpenAI Responses remain unchanged.
- Activation: `KILO_AUTO_RELIABILITY_ENABLED=true`. The default is disabled.
- Dirty-request compatibility activation: `KILO_AUTO_REQUEST_COMPAT_ENABLED=true`. The default is disabled and is independent from semantic retry behavior.

## Capability and policy profile

```yaml
channel_profile:
  id: kilo-auto-free
  public_protocols:
    openai_chat: true
    anthropic_messages: passthrough
    openai_responses: passthrough
  request_lifetime:
    one_shot: true
    reuse_upstream_sessions: false
  capabilities:
    chat: true
    stream: true
    tools: native
    json_mode: local_validate_repair
  limits:
    max_context_tokens: 256000
    runtime_input_budget: 200000
    context_safety_margin: 40000
    first_reasoning_max_tokens: 512
    repair_reasoning_max_tokens: 256
    repair_output_floor: 10000
  retry:
    raw_statuses: [429, 502, 503, 504]
    transport_before_headers: true
    raw_retry_backoff: [1s, 3s]
    total_wait_budget: 15s
    semantic_repairs: 1
    combined_max_upstream_calls: 4
    never_retry: [400, 413, 450, 451, 500]
```

## Request behavior

- With request compatibility enabled, only exact Kilo Auto OpenAI Chat requests are normalized. Recoverable flat function tools are wrapped; harmless empty/legacy Schema variants are repaired; structured or null tool-call arguments become JSON strings; unnamed tools are dropped only when `tool_choice` is absent, `auto`, or `none`; legacy `role:function` results become a matched tool result or an explicit flattened text result.
- Message content objects and legacy content blocks are normalized into standard text/image blocks. Standard `text`, `image_url`, and `file` blocks remain unchanged; `input_text` and supported image variants are mapped; unsupported audio/video blocks are removed while preserving sibling text, with a short placeholder only when media was the sole content.
- Trailing empty user placeholders are removed only after a matching assistant tool call and tool result. Recognizable flattened strict JSON Schema roots are moved under `json_schema.schema` and compiled by the existing validator.
- Forced or required tools without a name, genuinely empty user requests, non-empty malformed Schemas, unsupported tool/format combinations, and ambiguous legacy tool pairings still fail closed or are flattened without inventing a call ID. Invalid non-empty argument strings are passed through unchanged rather than guessed, so the upstream may accept or reject them.

- No-tool first attempts receive `reasoning.max_tokens=512` when absent or higher. Lower explicit limits and explicit reasoning disablement are preserved.
- Tool-bearing first attempts remain unchanged so the proven native tool path is protected.
- A repair keeps the full original conversation, tools, tool controls, and system/developer policy. It inserts one system completeness reminder before developer/user turns, appends a new final user retry instruction to encourage a fresh Auto route, sets `reasoning.max_tokens=256`, uses deterministic temperature, and raises only the repair output budget.
- Raw retries replay the exact normalized body. Raw retry allowance is shared across the first and repair attempt, preventing layered retry amplification.

## Response behavior

- Non-stream Chat is repaired once when the first choice has no visible content or valid tool call, has no finish reason, or ends with a length/output-limit finish.
- Content-policy/safety finish reasons are returned without semantic retry.
- Streaming holds a bounded prelude until the first visible content or tool-call delta. Once committed, the request is never replayed.
- A wholly empty/reasoning-only stream may be repaired once before headers or meaningful bytes are committed.
- Error events, policy finishes, and non-SSE responses are not semantically retried. A prelude exceeding 1 MiB is discarded before commitment and follows the same single-repair/error path as a wholly empty stream.
- Repair exhaustion returns the existing protocol-shaped generic public 500.

## Observability

- `kilo_auto_reliability`: stream flag, attempt count, repair usage, reasoning-cap decision, failure classes, final status, commit state, prelude overflow, and duration.
- `kilo_upstream_retry scope=auto`: path, public model, retry index, raw status or transport class, wait duration, and whether `Retry-After` was present.
- `kilo_auto_request_compat`: counts only for wrapped/dropped tools, merged legacy flat tool fields, repaired Schemas, normalized arguments/roles/content, dropped media blocks, placeholders, and trailing tool placeholders.
- `kilo_auto_400_class`: sanitized categories for message role/content, tool function/arguments/Schema/choice, sampling/output/stop fields, response format, paid auth, or unknown/non-JSON failures.
- Neither log line contains request/response bodies, prompt text, hidden reasoning, candidate answers, tool names/arguments, headers, or credentials.
