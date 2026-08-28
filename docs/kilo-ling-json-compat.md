# Kilo Structured JSON Compatibility

## Scope

- Models: `kilo/inclusionai/ling-3.0-tiny:free`, `kilo/inclusionai/ling-3.0-flash:free`, `kilo/stepfun/step-3.7-flash:free`, `kilo/kilo-auto/free`, `kilo/nvidia/nemotron-3-ultra-550b-a55b:free`, `kilo/nvidia/nemotron-3-super-120b-a12b:free`, `kilo/nvidia/nemotron-3.5-lightning:free`, `kilo/tencent/hy3:free`, `kilo/openrouter/free`, and `kilo/cohere/north-mini-code:free` (prefixed or unprefixed internal forms).
- Endpoint: OpenAI Chat Completions only.
- Formats: `json_object` and local strict `json_schema` validation.
- Ordinary requests, other models, Responses, Messages, and tools without `response_format` are unchanged.

## Behavior

- Removes the unsupported upstream format field only after capturing its intent. Kilo Auto and Nemotron Ultra/Super also omit it on the repair attempt so an unsupported native parameter cannot create an embedded HTTP-200 error envelope.
- First attempt keeps the original conversation and appends a compact final-format constraint.
- For StepFun and Kilo Auto only, when visible content is empty the adapter scans hidden reasoning locally and accepts only a JSON object that passes the requested validation. Reasoning text is never returned.
- Invalid output that cannot be salvaged gets one same-model repair turn that preserves system/developer policy and treats the original user request and candidate as quoted inert data. Nemotron Ultra/Super also use that one repair turn when their first structured attempt is reshaped into HTTP 400/500 from an embedded upstream error.
- Explicit low output limits use a 256-token first floor. Ling and Nemotron Ultra/Super use a 1024-token repair-only floor; StepFun and Kilo Auto use 4096 because their fallback may consume substantial hidden reasoning.
- Validated non-stream responses remove reasoning-related fields; SSE emits only canonical JSON content.
- Streaming is buffered upstream and emitted as standard SSE only after validation.
- Tools combined with `response_format`, malformed formats, external schema references, and final validation failures return the existing generic public error policy.
- Tencent HY3, OpenRouter Free, and Cohere North Mini Code are exact model profiles. Their ordinary requests and tools remain on the existing path; only Chat Completions requests with `response_format` enter the compatibility layer.

## Observability

Journal records use the `kilo_json_compat` prefix and contain only format, attempts, validation counts, reasoning-salvage/repair/stream flags, schema size, status, error class, and duration. They do not contain prompts, reasoning text, candidate text, headers, credentials, or raw upstream errors.

## Latest Gate

On 2026-08-06, the Ling isolated and live-wrapper gates covered ordinary chat, object/schema modes, empty-candidate reconstruction, non-streaming and streaming. Every 2xx structured response validated; accepted failures were upstream errors/timeouts rather than local JSON validation failures.

On 2026-08-07, StepFun was added after direct native probes repeatedly returned HTTP 400 for `json_object` and violated conflicting strict Schemas. Its focused local gate covers object repair, strict-Schema repair, buffered streaming, ordinary tools, combined tool/format rejection, and public-model preservation.

Kilo Auto and Nemotron Ultra/Super were later scoped from the full-site audit. Auto violated conflicting object and Schema instructions; Ultra/Super produced invalid or embedded-error responses for structured requests. Both reuse the same bounded, same-model validation and repair path while ordinary requests and tools remain unchanged.

On 2026-08-13, current production probes showed HTTP 200 invalid JSON for Tencent HY3, OpenRouter Free, Cohere North Mini Code, and the `kilo/openrouter/free` route. They were added as exact profiles using the same local validation, one bounded repair, buffered streaming, and fail-closed behavior; sibling models remain untouched.
