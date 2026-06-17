# Kilo NewAPI Adapter

Single-base adapter for putting Kilo's no-key gateway behind NewAPI while serving three client protocols:

- OpenAI Chat Completions: `/v1/chat/completions`
- OpenAI Responses: `/v1/responses`
- Anthropic Messages: `/v1/messages`

The adapter rewrites standard paths to Kilo gateway paths. It does not rewrite model names.

## Default Mapping

```text
/v1/chat/completions  -> https://api.kilo.ai/api/gateway/chat/completions
/v1/responses         -> https://api.kilo.ai/api/gateway/responses
/v1/messages          -> https://api.kilo.ai/api/gateway/messages
/v1/models            -> https://api.kilo.ai/api/gateway/models
```

The non-`/v1` variants are also accepted, for relays that strip the prefix.

`/v1/models` proxies Kilo's full upstream model list directly.

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

Use model IDs returned by `/v1/models`, for example `nvidia/nemotron-3-super-120b-a12b:free`.

## Environment

```text
ADDR=:18083
KILO_BASE_URL=https://api.kilo.ai/api/gateway
MAX_BODY_BYTES=10485760
UPSTREAM_TIMEOUT=180s
```

`KILO_API_KEY` is optional. It is not needed for the tested free models. Client `Authorization`, `Cookie`, and upstream override headers are stripped before forwarding to Kilo.

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
