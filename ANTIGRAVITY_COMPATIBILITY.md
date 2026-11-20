# Antigravity Protocol & Compatibility Matrix

> **Baseline Commit Audited:** `9b1d8aa` (`baseline-9b1d8aa`)  
> **Audit Date:** 2026-11-20  
> **Test Execution Status:** `PASS` (`go1.27.4 windows/amd64` — `go vet ./...`, `go vet -tags live_antigravity`, `go test -race -count=1 ./...`, `govulncheck ./...` all passed)  
> **Live Upstream Validation Status:** `VERIFIED (E2E via 1-Gateway Automation Engine)` — Live Antigravity account (`projectId: aicode-consumers`) tested against `daily-cloudcode-pa.googleapis.com` for proactive expired OAuth refresh via the 1-Gateway dashboard bridge (`expired` -> `healthy`, `3591s`), live `fetchAvailableModels` catalog sync (`36` models verified with MIME capabilities & quotas), `/v1/models` exposure, SSE streaming across 5 models/aliases (`LIVE_OK` / `LIVE_AUTO_OK`), `/v1/messages` streaming, parallel tool calls + `thoughtSignature` multi-turn, coding-agent session tracking, and base64 multimodal images. Standalone headless Go token refresh (`oauth2.googleapis.com/token` without `DASHBOARD_URL`) requires `ANTIGRAVITY_OAUTH_CLIENT_ID`/`SECRET` env vars.

## Protocol Classification

9router-go's Antigravity integration is **not** an officially documented Google public Gemini API integration. It is a **reverse-engineered compatibility layer** targeting Google's internal Cloud Code / Antigravity endpoints (`https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist`, `:onboardUser`, `:fetchAvailableModels`, `:generateContent`, `:streamGenerateContent`), paired with standard Google OAuth2 token refresh (`https://oauth2.googleapis.com/token`).

| Component | Category |
|---|---|
| `https://oauth2.googleapis.com/token` refresh (`internal/providers/oauth.go`) | Documented OAuth 2.0 refresh token grant (requires operator-supplied `ANTIGRAVITY_OAUTH_CLIENT_ID` / `ANTIGRAVITY_OAUTH_CLIENT_SECRET`) |
| Initial OAuth authorization flow | **HANDLED BY COMPANION DASHBOARD** in 1-Gateway mode (`DASHBOARD_URL`); **UNSUPPORTED** in standalone headless Go |
| `loadCodeAssist` / `onboardUser` / `fetchAvailableModels` (`internal/handlers/chat/antigravity_project.go`) | Undocumented / reverse-engineered Cloud Code internal RPCs |
| `v1internal:generateContent` & `:streamGenerateContent?alt=sse` (`internal/proxy/gemini.go`, `internal/translator/antigravity.go`) | Undocumented / reverse-engineered envelope (`project`, `model`, `userAgent`, `requestType`, `requestId`, `request`, `User-Agent: antigravity/hub/2.24.0 darwin/arm64`) |
| Tool cloaking (`_ide` suffix + 21 decoy tool declarations) | Compatibility heuristic — accepted by live upstream on multi-turn parallel tool calls |
| Model synonyms (`gemini-3.5/3.6/3.7/3.8-flash`, `gemini-3.1-pro`, `claude-sonnet-4-6`, `gemini-4-*`, `claude-*-5-5-*`) | Live-verified against `v1internal:fetchAvailableModels` (`2026-11-20`) |

---

## Claim Verification Matrix

Statuses follow strict verification rules:
- **VERIFIED**: Reproduced with live or executed test evidence in this session.
- **PARTIALLY VERIFIED**: Some scenarios work; others fail or lack evidence.
- **IMPLEMENTED**: Source code and/or unit tests exist in repository, but execution and/or live upstream validation is missing.
- **UNVERIFIED**: Insufficient evidence or unvalidated upstream behavior.
- **UNSUPPORTED**: Not implemented.
- **INCOMPATIBLE**: Confirmed protocol mismatch.

| Feature | Source Location | Test | Result | Evidence |
|---|---|---|---|---|
| OAuth Authorization Flow (Antigravity) | Companion Next.js dashboard via `DASHBOARD_URL` | — | **VERIFIED (1-Gateway)** / **UNSUPPORTED (standalone)** | Next.js interactive Google OAuth persisted valid AGY token + `projectId: aicode-consumers` into shared `data.sqlite` |
| OAuth Token Refresh (Antigravity) | `internal/autonomy/engine.go`, `internal/handlers/chat/gemini_handler.go`, `internal/providers/oauth.go` | Live startup sync + `TestEngine_FullSyncLifecycle` + `TestAntigravity_ConcurrentRefreshSerializedAndRotatesToken` | **VERIFIED (Live 1-Gateway Bridge + Unit/Race)** / **UNEXECUTED (Standalone Go without env creds)** | Automatically detected expired live token (`2026-11-20T05:05:51Z`) on startup, refreshed via loopback 1-Gateway dashboard bridge (`expired` -> `healthy`, `3591s`), and persisted rotated credentials to shared SQLite; standalone headless Go direct refresh requires `ANTIGRAVITY_OAUTH_CLIENT_ID`/`SECRET` |
| Project ID Discovery (`loadCodeAssist` / `onboardUser`) | `internal/handlers/chat/antigravity_project.go`, `internal/handlers/chat/gemini_handler.go` | `TestFetchAntigravityProjectID_outcomes`, `TestAntigravity_NoProjectID_ReturnsExplicitError` | **VERIFIED (mock)** | Removed fabricated `cloudcode-pa-<hex>` fallback; bounded reads (`1 MiB`) and context-aware `sleepProbeDelay` pass mock tests |
| Request Envelope & Basic Generation | `internal/translator/antigravity.go`, `internal/translator/gemini.go`, `internal/proxy/gemini.go`, `internal/proxy/executor/gemini.go` | Live `POST /v1/chat/completions`, `POST /v1/messages` | **VERIFIED (Live + Unit)** | Live `antigravity/gemini-3-flash` non-stream returned `200 OK` (`AGY_LIVE_OK`) and `/v1/messages` returned `200 OK` (`CLAUDE_TO_AGY_OK`) |
| SSE Streaming | `internal/handlers/chat/gemini_handler.go`, `internal/translator/gemini.go`, `internal/translator/response.go`, `internal/proxy/gemini.go` | Live `stream: true` on 5 models + `/v1/messages` | **VERIFIED (Live + Race)** | Fixed outdated `antigravity/ide/2.1.1` UA (which triggered Google deprecation banner) → `antigravity/hub/2.24.0 darwin/arm64`; live SSE returned `200 OK` + `[DONE]` on `gemini-3-flash`, `gemini-3.5-flash`, `gemini-3.1-pro`, `claude-sonnet-4-6`, `gemini-4-argon-medium`, and `/v1/messages` |
| Tool Calling, Parallel Tools & `thoughtSignature` | `internal/translator/antigravity.go`, `internal/translator/gemini.go`, `internal/translator/schema.go` | Live 2-turn parallel `get_weather` + `get_time` | **VERIFIED (Live + Race)** | Turn 1 returned 2 parallel tool calls (`call_get_weather_1__ts__...`, `call_get_time_2`, `finish_reason: "tool_calls"`); Turn 2 coalesced tool responses + `thoughtSignature` returned `200 OK` |
| 21 Decoy Tools Execution | `internal/translator/antigravity.go` | — | **UNSUPPORTED** | Declared only as `"This tool is currently unavailable."` decoy schemas; no tool execution exists in proxy |
| System Prompt Stripping | `internal/translator/antigravity.go` | `TestStripCompetitivePrompts` | **VERIFIED (Unit)** | Scoped `StripCompetitivePrompts` to `SystemInstruction` only so user `Contents` are never silently altered |
| Model Mapping & Live Catalog | `internal/translator/antigravity.go`, `internal/providers/capabilities.go` | Live `v1internal:fetchAvailableModels` + `TestNormalizeAntigravityModel_AllSynonymsValid` | **VERIFIED (Live + Unit)** | Replaced dead `gemini-pro-agent` (404) & deprecated aliases with live-verified targets (`gemini-3.8-flash-*`, `gemini-4-hydrogen-*`, `claude-sonnet-5-5-*`, `claude-opus-5-5-*`) |
| Multimodal (Image / Audio / PDF) | `internal/translator/gemini.go`, `internal/translator/request.go`, `internal/translator/antigravity.go` | Live base64 PNG + Unit tests | **PARTIALLY VERIFIED (Live base64 OK)** / **INCOMPATIBLE (HTTP `image_url`)** | Live 1x1 base64 PNG returned `200 OK` (`OK_IMG`); arbitrary HTTP `image_url` emits `fileData{mimeType: "image/*"}` which generally requires Google Files API URIs |
| Image Generation (`image_gen`) | `internal/translator/antigravity.go`, `internal/proxy/gemini.go` | `TestWrapAntigravityImageRequest` | **IMPLEMENTED (unit)** / **UNVERIFIED (live)** | Builds `requestType: "image_gen"` payload; only reads text prompt (ignores input image edits in `ForwardGemini`) |
| Proxy Pool Routing (HTTP/HTTPS/SOCKS5) | `internal/handlers/chat/connections.go`, `internal/db/proxyPools.go` | `TestGetClientForConnection_SupportsSOCKS5AndStrictProxyFailsClosed` | **VERIFIED (unit/race)** | Fixed bypass where `forwardGeminiNativeRequest` used `h.Client` instead of connection `httpClient`; added `https`/`socks5` support and `strictProxy` fail-closed |
| MITM Interception (Antigravity) | `internal/mitm/handlers/antigravity.go`, `internal/mitm/dns.go` | `internal/mitm/mitm_test.go` | **VERIFIED (unit)** (Windows **UNSUPPORTED**) | Linux/macOS `/etc/hosts` + local TLS; Windows explicitly returns unsupported error |
