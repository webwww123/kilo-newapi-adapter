# Kilo NewAPI Adapter Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go adapter that gives NewAPI one upstream base URL for Kilo Chat Completions, Responses, and Messages.

**Architecture:** A focused Go HTTP service handles path routing, sanitized upstream proxying, local count-token fallback, and simple health checks. Kilo already supports the three response protocols, so the adapter avoids unnecessary response conversion or model name mapping.

**Tech Stack:** Go standard library, `httptest` unit tests, Docker, systemd.

---

### Task 1: Request Routing And Model Pass-Through Tests

**Files:**
- Create: `go.mod`
- Create: `internal/adapter/config.go`
- Create: `internal/adapter/proxy.go`
- Test: `internal/adapter/proxy_test.go`

- [ ] Write tests for `/v1/chat/completions`, `/v1/responses`, `/v1/messages`, `/v1/models`, model pass-through, and non-`/v1` path variants.
- [ ] Verify tests fail before implementation.
- [ ] Implement route mapping and model pass-through.
- [ ] Verify tests pass.
- [ ] Commit routing behavior.

### Task 2: Header Sanitizing And Count Tokens

**Files:**
- Modify: `internal/adapter/proxy.go`
- Test: `internal/adapter/proxy_test.go`

- [ ] Write tests proving client auth/cookie headers are stripped and optional upstream bearer is added.
- [ ] Write tests for `POST /v1/messages/count_tokens`.
- [ ] Verify tests fail before implementation.
- [ ] Implement sanitized forwarding and approximate token counting.
- [ ] Verify tests pass.
- [ ] Commit proxy hardening behavior.

### Task 3: Executable, Docs, And Deployment Assets

**Files:**
- Create: `cmd/kilo-newapi-adapter/main.go`
- Create: `README.md`
- Create: `Dockerfile`
- Create: `deploy/kilo-newapi-adapter.service`
- Create: `.gitignore`

- [ ] Write or update tests covering health and server wiring where practical.
- [ ] Implement executable and graceful timeout defaults.
- [ ] Document NewAPI base URL, environment variables, and probe commands.
- [ ] Verify `go test ./...` passes.
- [ ] Commit executable and documentation.

### Task 4: Live Verification And Push

**Files:**
- Modify only if verification finds an issue.

- [ ] Run local adapter.
- [ ] Probe Chat Completions, Responses, and Messages through the adapter without upstream key.
- [ ] Stop local adapter.
- [ ] Review `git status`, `git diff --cached`, and secret exposure risk.
- [ ] Create GitHub repository and push `main`.
- [ ] Return repository link and verification summary.
