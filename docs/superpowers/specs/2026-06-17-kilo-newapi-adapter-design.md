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

## Model Names

The adapter does not rewrite, map, or inject public model names. Requests are forwarded with their original `model` value, and `/v1/models` proxies the full Kilo model list directly.

## Request Handling

The adapter validates JSON requests, then forwards the request body to Kilo without changing model names. Client `Authorization`, `Cookie`, and proxy/debug headers are not forwarded upstream. If `KILO_API_KEY` is configured, the adapter uses that value as the upstream bearer token.

Streaming bodies are proxied without protocol conversion. Kilo already returns Chat, Responses, and Messages shapes for the matching endpoint.

## Count Tokens

`POST /v1/messages/count_tokens` returns a local approximate Anthropic-compatible response:

```json
{"input_tokens": 123}
```

This prevents clients that preflight token counts from failing even when the upstream does not expose a count-token endpoint.

## Operations

The service includes `GET /healthz`, Docker support, and a systemd unit template. Logs are structural only: method, path, upstream path, status, duration, and byte count. Request bodies and secrets are not logged.
