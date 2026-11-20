# Security Policy & Audit Hardening

## Security Architecture Overview

`9router-go` is a local/private LLM API proxy gateway sharing a SQLite database (`data.sqlite`) with the 9Router dashboard.

### Key Security Controls

1. **Loopback Bind by Default (`cmd/9router-go/main.go`)**
   - Listens on `127.0.0.1:20128` by default (`HOST` env var).
   - Operators running inside containers must explicitly set `HOST=0.0.0.0` (configured in `docker-compose.yml`) and place the service behind an authenticated reverse proxy or firewall.
   - `http.Server` enforces `ReadHeaderTimeout: 15s` and global `middleware.MaxBody(10 MiB)`.

2. **API Key Authentication (`internal/middleware/auth.go`)**
   - All `/v1/*`, `/api/*` (except `/api/hello`), `/headroom/*`, `/proxy-pools/*`, and `/admin/*` routes require `RequireApiKey`.
   - Keys are accepted **only** via `Authorization: Bearer <key>` or `X-API-Key: <key>` headers. Query-string keys (`?key=`) are rejected to prevent credential leakage in access logs and referrers.

3. **OAuth Security (`internal/providers/oauth.go`, `internal/handlers/oauth/oauth.go`, `internal/handlers/chat/gemini_handler.go`)**
   - No embedded Google or provider OAuth client secrets are shipped in source code. Operators must provide credentials via environment variables (`ANTIGRAVITY_OAUTH_CLIENT_ID`, `ANTIGRAVITY_OAUTH_CLIENT_SECRET`, etc.).
   - Token refresh uses per-connection mutex serialization to prevent concurrent refresh-token race conditions and persists rotated `refreshToken` values.
   - Kiro social OAuth exchange enforces single-use server-side `state` + PKCE `code_verifier` validation, URL-encoded form bodies, and never returns raw `access_token` / `refresh_token` / `id_token` values in HTTP responses.
   - Upstream OAuth error bodies are truncated to 200 bytes in error messages to prevent echoed token leakage.

4. **SSRF & Open-Proxy Protections**
   - **Dial-Time SSRF Transport (`internal/handlerutil/ssrf.go`)**: `NewSSRFSafeTransport()` wraps `net.Dialer.Control` to reject loopback, RFC 1918 private, link-local (`169.254.0.0/16`), and unique-local IPv6 addresses on the resolved TCP peer address at socket connect time, eliminating DNS-rebinding TOCTOU gaps.
   - **Headroom Proxy (`internal/handlers/media/headroom.go`)**: Non-loopback `headroomUrl` targets are validated with `handlerutil.AssertPublicURL` and dispatched via `NewSSRFSafeTransport()` (`SafeClient`), with responses capped at 10 MiB.
   - **Edge Relay Templates (`internal/handlers/media/deploy.go`)**: Deployed Vercel, Cloudflare Worker, and Deno relay scripts require a per-deployment random `x-relay-secret` header and reject non-HTTPS, localhost, RFC 1918, IPv6 ULA/link-local, and `169.254.x.x` targets.
   - **Egress Proxy Pools (`internal/handlers/chat/connections.go`)**: Supports `http`, `https`, `socks5`, and `socks5h`; enforces `strictProxy` fail-closed behavior; redacts proxy credentials from logs via `url.URL.Redacted()`.
   - **MITM Proxy (`internal/mitm/server.go`)**: Binds strictly to `127.0.0.1:443` and drops bodies exceeding 10 MiB.

5. **Database & File Permissions (`internal/db/client.go`, `internal/db/accounts.go`, `internal/db/crypto.go`)**
   - SQLite database directory is restricted to `0700` and database file to `0600` on open; per-connection PRAGMAs (`busy_timeout=5000`, `WAL`, `foreign_keys=ON`) are enforced via DSN so all pool connections share identical concurrency/integrity settings.
   - **Optional Credential Encryption at Rest (`CREDENTIAL_ENCRYPTION_KEY`)**: When `CREDENTIAL_ENCRYPTION_KEY` is configured (64-hex raw key, base64 32-byte key, or passphrase), secret fields in `providerConnections.data` (`apiKey`, `accessToken`, `refreshToken`, `idToken`, `cookie`, `pat`, `token`) are encrypted with AES-256-GCM (`enc:v1:...`), existing plaintext rows are migrated on startup (`MigrateEncryptConnections`), and encrypted rows refuse to fall back silently if the key is missing. When unset (for Next.js dashboard SQLite sharing), plaintext mode remains backward-compatible and emits a startup warning if `HOST` is non-loopback.
   - All SQL queries use parameterized placeholders; `json_set` paths quote model names (`$."modelLock_<model>"`) to prevent JSON path injection.

6. **Self-Updater Integrity & Secret Hygiene (`internal/updater/updater.go`, `internal/proxy/executor/qoder.go`)**
   - `CheckUpdate` and `PerformSelfUpdateVerified` require `https://` + `AssertPublicURL` + `NewSSRFSafeTransport()` (overridable only via `ALLOW_INSECURE_UPDATE=true`), cap manifests at `1 MiB` and binaries at `128 MiB`, and refuse self-updates without SHA-256 checksum verification (`ALLOW_UNVERIFIED_UPDATE=true` to bypass).
   - `qoder` PAT-to-jobToken cache keys are SHA-256 digests (`crypto/sha256`) rather than raw credentials in map keys.

## Known Security Limitations

- **No Built-in Rate Limiter on Proxy Endpoints**: Operators exposing `9router-go` beyond localhost should enforce rate limiting at a reverse proxy (nginx/Caddy/Cloudflare).
- **Plaintext Mode Without `CREDENTIAL_ENCRYPTION_KEY`**: Required when sharing `data.sqlite` directly with an unencrypted Next.js dashboard process; Linux server deployments should set `CREDENTIAL_ENCRYPTION_KEY`.

## Reporting Vulnerabilities

Please report security issues privately to the repository maintainer rather than opening public issues with exploit payloads or credentials.
