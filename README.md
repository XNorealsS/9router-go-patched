# 9router-go

[![CI](https://github.com/luqman-v1/9router-go/actions/workflows/ci.yml/badge.svg)](https://github.com/luqman-v1/9router-go/actions/workflows/ci.yml)
[![Release](https://github.com/luqman-v1/9router-go/actions/workflows/release.yml/badge.svg)](https://github.com/luqman-v1/9router-go/actions/workflows/release.yml)

High-performance Go proxy gateway for [9Router](https://github.com/decolua/9router) LLM routing.

> **9Router** is a local AI routing gateway + dashboard. This Go proxy replaces the Next.js `/v1/*` routes for high-throughput LLM traffic, while the [9Router dashboard](https://github.com/decolua/9router) handles management UI (providers, API keys, combos, usage tracking).

## 1. What This Project Does

- Routes OpenAI (`/v1/chat/completions`, `/v1/responses`), Anthropic (`/v1/messages`), and Ollama (`/api/chat`) requests to configured upstream LLM providers.
- Shares the SQLite WAL database (`data.sqlite`) with the 9Router dashboard for API keys, provider connections, model aliases, combos, usage logs, and proxy pools.
- Translates requests/responses/SSE streams across OpenAI, Anthropic Claude, and Gemini-native formats, with per-connection model cooldown locks, combo strategies (`fallback`, `round-robin`, `sticky`, `fusion`), and OAuth token refresh.
- Supports optional token-saving preprocessors (`RTK`, `Caveman`, `Ponytail`), SSE stall detection, egress proxy pools (`http`, `https`, `socks5`), and real-time SSE usage streaming (`/api/usage/stream`).

## 2. What This Project Does Not Do

- Does **not** ship a web dashboard UI (use the 9Router Next.js dashboard or populate SQLite directly).
- Does **not** implement Antigravity initial OAuth login/authorization (only token refresh and SQLite/API token import).
- Does **not** guarantee account safety, "anti-ban" protection, unlimited quota, or official Google Antigravity parity.
- Does **not** execute the 21 declared Antigravity decoy tools (they exist only as compatibility schema stubs).
- Does **not** run MITM interception on Windows (`internal/mitm/dns.go` supports Linux/macOS `/etc/hosts` only).

## 3. Provider & Antigravity Verification Status

- See [`ANTIGRAVITY_COMPATIBILITY.md`](ANTIGRAVITY_COMPATIBILITY.md) for the complete claim verification matrix.
- OpenAI-compatible forwarding, SSE streaming, error classification, combo fallback, and SQLite locking are covered by unit and mock-server integration tests in `internal/`.
- Live upstream provider behavior (including Antigravity `daily-cloudcode-pa.googleapis.com`) is **NOT** live-verified in this repository without operator-supplied credentials and Go test execution (`BLOCKED` in environments without Go or authorized credentials).

## 4. Experimental Features

- **Antigravity Gemini-native compatibility layer** (`_ide` tool cloaking, 21 decoy tool stubs, system-instruction prompt stripping, model synonyms).
- **Provider-specific reverse-engineered executors**: Qoder COSY signing, CodeBuddy CN/INTL SSE aggregation, Trae SOLO remote agent, Windsurf gRPC-web.
- **MITM local TLS proxy** (`9router-go mitm enable`) for intercepting local CLI tools on Linux/macOS.
- **Headroom lifecycle manager & reverse proxy** (`/headroom/*`).

## 5. Capabilities Requiring Authorized Accounts

- All upstream LLM calls (except explicitly configured `NoAuth` public endpoints) require valid API keys or authorized OAuth accounts in `providerConnections`.
- OAuth token refresh requires operator-configured OAuth client credentials via environment variables (no Google or provider `client_secret` values are embedded in source code).

## 6. How OAuth Works

- `9router-go` reads `accessToken`, `refreshToken`, `expiresAt`, and `projectId` from `providerConnections.data` in SQLite (populated by the 9Router dashboard or `POST /api/oauth/{provider}/import`).
- When a token expires (within 60s buffer) or upstream returns `401 Unauthorized`, `ChatHandler` acquires a per-connection mutex, refreshes via `oauth.Get(provider)` or `providers.KnownOAuthConfigs[provider]`, and persists the new `accessToken`, `expiresAt`, and any rotated `refreshToken` to SQLite.
- Kiro social OAuth (`/api/oauth/kiro/social-authorize` and `/social-exchange`) uses PKCE (`S256`) and single-use server-side `state` validation, storing tokens in SQLite without returning raw tokens in HTTP responses.

## 7. How to Deploy Securely

- By default, `9router-go` binds to `127.0.0.1:20128` (`HOST=127.0.0.1`). Keep it bound to loopback when running locally.
- In containers (`docker-compose.yml`), set `HOST=0.0.0.0` only behind an authenticated reverse proxy or private network.
- Configure `JWT_SECRET`, `INITIAL_PASSWORD`, and provider `*_OAUTH_CLIENT_ID` / `*_OAUTH_CLIENT_SECRET` via environment variables or a `0600` `.env` file.
- See [`SECURITY.md`](SECURITY.md) for full security hardening details, SSRF guards, and edge relay secret protection.

## 8. How to Run Tests

```bash
# Static check and all unit / mock integration tests
go vet ./...
go test ./... -count=1

# Race detector
go test -race ./...

# Live Antigravity validation (requires authorized operator credentials)
ANTIGRAVITY_OAUTH_CLIENT_ID=... \
ANTIGRAVITY_OAUTH_CLIENT_SECRET=... \
ANTIGRAVITY_LIVE_REFRESH_TOKEN=... \
go test -tags=live_antigravity -v ./internal/handlers/chat -run TestLiveAntigravity
```

## 9. Known Limitations

- **SQLite single-writer daily aggregation**: `upsertDailyUsage` merges JSON blobs in-process; running multiple `9router-go` processes against one DB file can drop concurrent daily stat increments.
- **No HTTP rate limiter**: Enforce request rate limiting at your reverse proxy if exposed beyond localhost.
- **Unverified Antigravity model catalog**: Synonyms such as `gemini-3.5-flash` / `gemini-3.6-flash` / `gemini-3.7-flash` are compatibility mappings and may return upstream 404 depending on account entitlements.

## 10. Security Considerations

- SQLite `providerConnections.data` stores upstream API keys and OAuth tokens in plaintext for dashboard compatibility; the DB file is `chmod 0600` on open (POSIX).
- Edge relay deployments (`vercel`, `cloudflare`, `deno`) generate a per-deployment `x-relay-secret` and block private/loopback/metadata targets, preventing open-relay abuse.
- Review [`SECURITY.md`](SECURITY.md) before exposing any endpoint outside localhost.

### Features

- **High-throughput Go runtime**: CGO-free binary using `chi/v5` and `modernc.org/sqlite` WAL mode (see [`benchmark/RESULTS.md`](benchmark/RESULTS.md) for measured mock-upstream numbers: 5,920 RPS non-stream / 5,437 RPS stream via `hey` with 5ms mock latency, up to 13,216 RPS via native runner with 2ms mock latency; do not extrapolate to live provider latency)
- **SQLite WAL mode** with non-blocking concurrency (shared with [9Router dashboard](https://github.com/decolua/9router))
- **OpenAI, Claude, and Gemini native format support** with bidirectional SSE translation
- **Antigravity Compatibility Layer**: `_ide` tool suffix cloaking, 21 decoy tool stubs, system-instruction prompt stripping, and protobuf schema sanitization (experimental; see [`ANTIGRAVITY_COMPATIBILITY.md`](ANTIGRAVITY_COMPATIBILITY.md))
- **Dynamic Egress Proxy Pools & Edge Relays**: round-robin IP rotation via active `http`/`https`/`socks5` pools + secret-authenticated Vercel/Cloudflare/Deno edge relays (`x-relay-target` / `x-relay-path` / `x-relay-secret`)
- **No-Auth Provider Proxy Strategies**: automatic proxy pool routing & rotation for free-tier/public providers (`settings.providerStrategies`)
- **Realtime SSE Usage Stream (`/api/usage/stream`)**: in-memory in-flight request tracker powering live telemetry on the Next.js Usage Topology graph
- **Snake_case Token Limits (`/v1/models` & `/v1/models/info`)**: exposes `context_length`, `max_completion_tokens`, `max_input_tokens`, and `max_output_tokens`
- **Gemini Multimodal Vision, Audio & PDF**: support for base64 inline images, audio (`input_audio`), and Claude `document` (PDF) blocks
- **Dedicated Executors**: Qoder COSY signing, CodeBuddy CN/INTL streaming, Trae SOLO remote agent, Windsurf gRPC-web
- **Combo strategies**: sticky round-robin, round-robin, fallback, fusion (multi-panel + judge)
- **Auto-capability-switch**: floats vision/pdf/audio-capable models to front based on request content
- **Turn & Tool-Calling Stickiness**: locks multi-turn tool calling to the same provider/model to preserve thought signatures
- **Error classification & Per-connection model locks**: text/status rules + exponential backoff DB-compatible with Next.js
- **SSE stall detection**: 6-minute timeout with per-chunk reset
- **Reactive 401 Unauthorized Auto-Refresh**: serialized per-connection OAuth refresh with refresh-token rotation persistence
- **Token savers**: RTK input compression, Caveman terse output, Ponytail minimal-code bias
- **Headroom Lifecycle Proxy**: token compression proxy management & SSRF-guarded dashboard reverse proxy
- **Live Console Logs**: in-process ring buffer + SSE streaming for dashboard console monitoring

## Architecture

```
┌─────────────────┐     ┌──────────────────────┐     ┌─────────────────┐
│   CLI Client    │────▶│    Go Proxy           │────▶│  Upstream LLM   │
│  (Claude Code,  │     │                       │     │  (OpenAI, etc.) │
│   Codex, etc.)  │     │  • Auth (SQLite)      │     └─────────────────┘
│                 │     │  • Model resolution   │
│                 │     │  • Combo strategies   │
│                 │     │    - sticky           │
│                 │     │    - round-robin      │
│                 │     │    - fallback         │
│                 │     │    - fusion           │
│                 │     │  • Auto-capability    │
│                 │     │  • SSE streaming      │
│                 │     │  • Stall detection    │
│                 │     │  • Error klasifikasi  │
│                 │     │  • Translation        │
│                 │     └───────┬──────────┘
│                             │
└─────────────────────┐     ┌─▼──────────────────┐
  │   Dashboard     │────▶│  SQLite (WAL)    │
  │  [9Router]      │     └────────────────────┘
  │  • Providers    │
  │  • API Keys     │
  │  • Usage        │
  └─────────────────┘
```

### Request Flow

```
Client → Auth → resolveModel() → [Combo?]
    │                              │
    │ Yes                          │ No
    ▼                              ▼
Combo Handler                 Single Model
    │                              │
    ├─ sticky/round-robin          │
    ├─ fallback                    │
    └─ fusion (parallel panel)     │
    │                              │
    ▼                              ▼
detectRequiredCapabilities()
    │
    ▼
tryForwardWithConnection()
    │
    ├─ Success → unlockModel + logUsage
    └─ Error  → classifyError() → lockConnectionModel()
                                       │
                                  Fallback model?
                                       │ Yes → retry next model
                                       │ No  → error response
```

See [ARCHITECTURE.md](ARCHITECTURE.md) for detailed flow diagrams (combo, fusion, error classification, locking, SSE stall, etc.).

## Quick Start

```bash
# Build
go build -o 9router-go ./cmd/9router-go/

# Run (standalone, no dashboard needed)
PORT=20128 ./9router-go

# Health check
curl http://localhost:20128/health
```

## Combo Strategies

Combo models support multiple routing strategies, configurable per combo:

| Strategy | Description |
|----------|-------------|
| **fallback** | Try models in order, skip on error (default) |
| **round-robin** | Rotate starting index per request |
| **sticky** | Round-robin with consecutive-use pinning; rotate after `stickyLimit` requests |
| **fusion** | Fire all panel models in parallel → collect with quorum grace → judge synthesizes final answer |

All strategies support **auto-capability-switch**: if the request body contains images or PDFs, capable models (OpenAI, Anthropic, Gemini, etc.) are floated to the front automatically.

### Fusion

Fusion runs multiple models as a panel in parallel:

1. **Fan-out**: Send request to all panel models simultaneously (non-streaming)
2. **CollectPanel**: Wait for quorum (`minPanel=2`), apply `stragglerGraceMs=8s`, hard timeout at `panelHardTimeoutMs=90s`
3. **Degrade gracefully**: If 0 answers → 503; if 1 answer → answer directly
4. **Judge synthesis**: Build anonymized panel responses → send to judge model → final answer streamed to client

## Error Classification

Errors are classified using the same rule system as Next.js:

| Rule | Type | Action |
|------|------|--------|
| `"no credentials"` | Text | Cooldown 120s |
| `"request not allowed"` | Text | Cooldown 5s |
| `"rate limit"` | Text | Exponential backoff |
| `"too many requests"` | Text | Exponential backoff |
| `"quota exceeded"` | Text | Exponential backoff |
| `"capacity"` / `"overloaded"` | Text | Exponential backoff |
| 401 / 402 / 403 / 404 | Status | Cooldown 120s |
| 429 | Status | Exponential backoff |
| Default (unmatched) | — | Cooldown 30s |

**Exponential backoff**: 2s base, doubled per level, max 5 minutes, 15 levels max.
Backoff level is tracked per-connection in `providerConnections.data.backoffLevel` (DB-compatible with Next.js dashboard).

## Model Locking

**Per-connection model locks** — stored as `modelLock_<model>` fields in `providerConnections.data` JSON blob.
Same format as Next.js, readable by the shared dashboard.

- Failed connection → `LockConnectionModel(id, model, duration)` → `data.modelLock_gpt-4 = "ISO timestamp"`
- Successful request → `UnlockConnectionModel(id, model)` → `data.modelLock_gpt-4 = null`, `backoffLevel = 0`
- Connection selection → skips connections with active model lock

## SSE Stall Detection

Each SSE stream is wrapped with a `StallReader` (6-minute timeout by default).

- Timer resets on each received chunk
- If timer fires (no data for 6 minutes) → underlying connection is closed → `Read` unblocks with error → stream terminated
- No goroutine leak on clean stream close (timer stopped)

## Token Savers

Reduce token usage on routed LLM traffic. Each saver is independently toggleable
via CLI flag or environment variable (CLI flag overrides env).

| Saver | CLI flag | Env var | Default | Effect |
|-------|----------|---------|---------|--------|
| RTK | `--rtk` | `RTK_ENABLED` | **on** | Content-aware compression of tool/tool_result messages (git diff, logs, grep, tree) |
| Caveman | `--caveman` | `CAVEMAN_ENABLED` | off | Injects terse-output system prompt (~65% fewer output tokens) |
| Ponytail | `--ponytail` | `PONYTAIL_ENABLED` | off | Injects lazy-senior-dev prompt biasing minimal code |

```bash
# All savers on
./9router-go --rtk --caveman --ponytail
```

> RTK is on by default. Disable with `RTK_ENABLED=false` or `--rtk=false`.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `20128` | Server port |
| `DATA_DIR` | `~/.9router/` | Data directory (DB, JWT secret) |
| `DB_PATH` | `DATA_DIR/db/data.sqlite` | Custom SQLite DB path (overrides DATA_DIR) |
| `LOG_FILE` | stderr | Log output file (defaults to stderr when unset) |
| `RTK_ENABLED` | `true` | Enable RTK input compression |
| `CAVEMAN_ENABLED` | `false` | Enable Caveman terse output style |
| `PONYTAIL_ENABLED` | `false` | Enable Ponytail minimal-code bias |

## Database

Uses the same SQLite DB as [9Router dashboard](https://github.com/decolua/9router) (`~/.9router/db/data.sqlite`) with WAL mode.

**Tables:** `apiKeys`, `providerConnections`, `providerNodes`, `combos`, `kv`, `settings`, `usageHistory`, `usageDaily`, `requestDetails`, `proxyPools`, `_meta`

See [DATABASE.md](DATABASE.md) for full schema documentation, JSON blob structure, and Go vs Next.js differences.

### Custom DB Location

```bash
# Use custom SQLite path
DB_PATH=/mnt/shared/9router/data.sqlite PORT=20128 ./9router-go
```

## API Endpoints

```
# Core Chat & Completion Endpoints
POST /v1/chat/completions      # OpenAI format
POST /v1/messages              # Claude format
POST /v1/messages/count_tokens # Claude token counter
POST /v1/embeddings            # Embeddings
POST /v1/responses             # Responses API
POST /v1/responses/compact     # Compact responses API
POST /api/chat                 # Ollama compatible format

# Media & Multimodal Endpoints
POST /v1/images/generations    # Text-to-image generation
POST /v1/videos/generations    # Video generation
POST /v1/videos/edits          # Video edits
POST /v1/videos/extensions     # Video extension
GET  /v1/videos/{id}           # Video status lookup
POST /v1/audio/speech          # Text-to-speech (TTS)
POST /v1/audio/transcriptions  # Speech-to-text (STT)
GET  /v1/audio/voices          # TTS voices list

# Search, Scrape & Web Fetch
POST /v1/search                # Web search (provider-selected)
POST /v1/scrape                # Web scrape
POST /v1/web/fetch             # Web URL extraction

# Models & Token Limits
GET  /v1/models                # List models (with context_length, token limits)
GET  /v1/models/info           # Model capability & limits metadata
GET  /v1/models/{kind}         # Models filtered by kind (e.g. tts, image)

# Usage & Realtime SSE Stream
GET  /api/usage/stream         # Live SSE in-flight request tracking & topology animation
GET  /api/usage/stats          # Realtime usage stats & active concurrency

# OAuth Token Import & Kiro Social Exchange
POST /api/oauth/{provider}/import     # Import API key or OAuth tokens into SQLite
GET  /api/oauth/kiro/social-authorize # Kiro Cognito PKCE authorize URL + state
POST /api/oauth/kiro/social-exchange  # Kiro Cognito PKCE token exchange
POST /api/oauth/codex/bulk-import     # Bulk Codex access token import

# System & Monitoring
GET  /health                          # Health check
GET  /api/version                     # Proxy version & update check
GET  /translator/console-logs/stream  # Dashboard live console log SSE stream
```

## Docker

### Pull from Docker Hub

```bash
docker pull luqmenul/9router-go:latest
```

### Docker Compose (`docker-compose.yml`)

#### With Outbound Egress Proxy (Microwarp SOCKS5)

```yaml
services:
  microwarp:
    image: ghcr.io/ccbkkb/microwarp:latest
    container_name: microwarp
    restart: always
    ports:
      - "1080:1080"
    cap_add:
      - NET_ADMIN
      - SYS_MODULE
    sysctls:
      - net.ipv4.conf.all.src_valid_mark=1
    volumes:
      - ./warp:/etc/wireguard

  9router-go:
    image: luqmenul/9router-go:latest
    container_name: 9router-go
    ports:
      - "20130:20128"
    environment:
      - PORT=20128
      - DATA_DIR=/data
      - RTK_ENABLED=true
      - CAVEMAN_ENABLED=false
      - PONYTAIL_ENABLED=true
      - HTTP_PROXY=socks5://microwarp:1080
      - HTTPS_PROXY=socks5://microwarp:1080
    volumes:
      - ./data:/data
    depends_on:
      - microwarp
    restart: unless-stopped
```

#### Standalone Deployment

```yaml
services:
  9router-go:
    image: luqmenul/9router-go:latest
    container_name: 9router-go
    ports:
      - "20128:20128"
    environment:
      - PORT=20128
      - DATA_DIR=/data
      - RTK_ENABLED=true
      - CAVEMAN_ENABLED=false
      - PONYTAIL_ENABLED=true
    volumes:
      - ./data:/data
    restart: unless-stopped
```

```bash
# Start container
docker compose up -d
```

## Cross-Compile

```bash
GOOS=linux GOARCH=amd64 go build -o 9router-go-linux ./cmd/9router-go/
GOOS=darwin GOARCH=arm64 go build -o 9router-go-mac ./cmd/9router-go/
GOOS=windows GOARCH=amd64 go build -o 9router-go.exe ./cmd/9router-go/
```

## Test

```bash
go vet ./...
go test ./... -count=1
go test -race ./...
```

Note: Live Antigravity integration tests (`-tags=live_antigravity`) require operator-supplied OAuth credentials and are skipped unless explicitly configured.

## Benchmark

Run the native self-contained Go benchmark runner (mock upstream, zero external dependencies):

```bash
go run ./benchmark/runner.go
```

Note: The figures in [`benchmark/RESULTS.md`](benchmark/RESULTS.md) were recorded on Apple Silicon against a local mock upstream (2–5ms artificial delay) and measure proxy overhead only, not end-to-end live provider latency.

## Roadmap

See [`ROADMAP.md`](ROADMAP.md) for planned future features, cost-aware model routing, semantic caching, and alerting proposals.

## Credits

- [9Router](https://github.com/decolua/9router) — Original Next.js LLM routing gateway + dashboard
