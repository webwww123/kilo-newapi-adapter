# Kilo NewAPI Adapter Design

## Goal

Expose one NewAPI-friendly upstream base URL that can serve OpenAI Chat Completions, OpenAI Responses, and Anthropic Messages clients while proxying to Kilo's no-key gateway.

## Architecture

The adapter is a small Go HTTP service. NewAPI points at the adapter as a single base URL. The adapter routes standard client paths to the matching Kilo gateway path:

- `POST /v1/chat/completions` -> `POST /api/gateway/chat/completions`
- `POST /v1/responses` -> `POST /api/gateway/responses`
- `POST /v1/messages` -> `POST /api/gateway/messages`
- `GET /v1/models` -> `GET /api/gateway/models`

Non-`/v1` variants are also accepted for compatibility with relays that strip the prefix.

## Model Aliases

The default public model alias is `v1chat`. It rewrites to `nvidia/nemotron-3-super-120b-a12b:free`, which was probed successfully for all three protocols without an upstream key.

Aliases are configurable with `MODEL_ALIASES`, for example:

```text
MODEL_ALIASES=v1chat=nvidia/nemotron-3-super-120b-a12b:free,kilo-free=kilo-auto/free
```

If a request has no `model`, the adapter inserts `DEFAULT_MODEL`, defaulting to `v1chat`.

## Request Handling

The adapter parses JSON requests, rewrites the top-level `model` field when it matches an alias, then forwards the request body to Kilo. Client `Authorization`, `Cookie`, and proxy/debug headers are not forwarded upstream. If `KILO_API_KEY` is configured, the adapter uses that value as the upstream bearer token.

Streaming bodies are proxied without protocol conversion. Kilo already returns Chat, Responses, and Messages shapes for the matching endpoint.

## Count Tokens

`POST /v1/messages/count_tokens` returns a local approximate Anthropic-compatible response:

```json
{"input_tokens": 123}
```

This prevents clients that preflight token counts from failing even when the upstream does not expose a count-token endpoint.

## Operations

The service includes `GET /healthz`, Docker support, and a systemd unit template. Logs are structural only: method, path, upstream path, status, duration, and byte count. Request bodies and secrets are not logged.

