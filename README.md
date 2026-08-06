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
```

`KILO_API_KEY` is optional. It is not needed for the tested free models. Client `Authorization`, `Cookie`, and upstream override headers are stripped before forwarding to Kilo.

## Public Error Policy

- Every client-visible 4xx response is collapsed to HTTP 400 with `400 请求无法处理，请检查请求参数或稍后重试。`.
- Every client-visible 5xx response is collapsed to HTTP 500 with `500 请求无法处理，请检查请求参数或稍后重试。`.
- HTTP 200 error envelopes and SSE error events use the same generic 500 message.
- Original adapter and upstream error details remain in the root-only request log for internal debugging and are never returned to clients.

## Free Model Context Trimming

Context trimming is intentionally limited to manually profiled free models. Paid models and unknown models pass through unchanged.

The trim policy keeps system/developer items, the originating user request, and the newest completed tool turn first. OpenAI `assistant.tool_calls` plus `tool` results, Anthropic `tool_use` plus `tool_result` blocks, and Responses `function_call` plus `function_call_output` items are retained atomically. Oversized newest tool results are truncated while preserving both edges before older conversation turns are considered. If the required current tool structure still cannot fit, the adapter leaves the original request history intact so the upstream can return an explicit context error instead of receiving stale partial history.

Current free-model profiles are maintained in `internal/adapter/context_trim.go`. `stepfun/step-3.7-flash:free` follows the local Yingweida CPA middleware profile: 262144 context, no safety margin, and 220000 max input tokens.
`inclusionai/ling-3.0-flash:free` uses its advertised 262144 context window with a 40000-token safety margin and a 200000-token maximum input budget.

Ling requests are normalized locally before they reach Kilo. Image, audio, video, and
Embeddings requests are rejected locally with the generic public 400 error. Unsupported reasoning-effort
controls are removed. Chat Completions `json_object` and strict `json_schema` requests are captured before
the upstream cleanup step and emulated in-process only for Ling: the adapter validates the completed object,
repairs an invalid or empty first answer once, and buffers client streaming until validation succeeds.
Requests combining tools with `response_format` are rejected locally; ordinary tools and requests without
`response_format` stay on the existing path. Explicit `max_tokens`, `max_completion_tokens`,
and `max_output_tokens` values up to 32768 are preserved; larger values are removed so Kilo can
apply its model/provider default instead of returning a generic upstream 400. If an oversized
tool or externally stateful request cannot be trimmed without breaking call/result pairing,
the adapter rejects it locally instead of forwarding stale or partial history; the detailed
reason remains available in the internal request log.

Kilo's provider pool for `stepfun/step-3.7-flash:free` inconsistently rejects OpenAI
`response_format: {"type":"json_object"}` requests. For Chat Completions, the adapter
therefore removes that upstream field and appends an equivalent JSON-only system instruction.
Other models, response format types, request fields, messages, and streaming settings are left unchanged.

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
