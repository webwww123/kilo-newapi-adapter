# Kilo Ling JSON Compatibility

## Scope

- Model: `kilo/inclusionai/ling-3.0-flash:free` (prefixed or unprefixed internal form).
- Endpoint: OpenAI Chat Completions only.
- Formats: `json_object` and local strict `json_schema` validation.
- Ordinary requests, other models, Responses, Messages, and tools without `response_format` are unchanged.

## Behavior

- Removes the unsupported upstream format field only after capturing its intent.
- First attempt keeps the original conversation and appends a compact final-format constraint.
- Invalid or empty output gets one repair turn that preserves system/developer policy and treats the original user request and candidate as quoted inert data.
- Explicit low output limits use a 256-token first floor and a 1024-token repair-only floor.
- Streaming is buffered upstream and emitted as standard SSE only after validation.
- Tools combined with `response_format`, malformed formats, external schema references, and final validation failures return the existing generic public error policy.

## Observability

Journal records use the `kilo_ling_json_compat` prefix and contain only format, attempts, validation counts, repair/stream flags, schema size, status, error class, and duration. They do not contain prompts, candidate text, headers, credentials, or raw upstream errors.

## Latest Gate

On 2026-08-06, the isolated and live-wrapper gates covered ordinary chat, object/schema modes, empty-candidate reconstruction, non-streaming and streaming. Every 2xx structured response validated; accepted failures were upstream errors/timeouts rather than local JSON validation failures.
