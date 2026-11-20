# Antigravity Protocol & Compatibility Matrix

> **Baseline Commit Audited:** `9b1d8aa` (`baseline-9b1d8aa`)  
> **Audit Date:** 2026-11-20  
> **Live Validation Status:** `BLOCKED` — Go toolchain (`go` 1.26.0) is not installed in this environment, and no authorized Antigravity OAuth credentials are configured.

## Protocol Classification

9router-go's Antigravity integration is **not** an officially documented Google public Gemini API integration. It is a **reverse-engineered compatibility layer** targeting Google's internal Cloud Code / Antigravity endpoints (`https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist`, `:onboardUser`, `:generateContent`, `:streamGenerateContent`), paired with standard Google OAuth2 token refresh (`https://oauth2.googleapis.com/token`).

| Component | Category |
|---|---|
| `https://oauth2.googleapis.com/token` refresh (`internal/providers/oauth.go`) | Documented OAuth 2.0 refresh token grant (requires operator-supplied `ANTIGRAVITY_OAUTH_CLIENT_ID` / `ANTIGRAVITY_OAUTH_CLIENT_SECRET`) |
| Initial OAuth authorization flow | **UNSUPPORTED** in `9router-go` (delegated to Next.js dashboard / external CLI; Go proxy reads/imports tokens from SQLite `providerConnections`) |
| `loadCodeAssist` / `onboardUser` (`internal/handlers/chat/antigravity_project.go`) | Undocumented / reverse-engineered Cloud Code internal RPCs |
| `v1internal:generateContent` & `:streamGenerateContent?alt=sse` (`internal/proxy/gemini.go`, `internal/translator/antigravity.go`) | Undocumented / reverse-engineered envelope (`project`, `model`, `userAgent`, `requestType`, `requestId`, `request`) |
| Tool cloaking (`_ide` suffix + 21 decoy tool declarations) | Compatibility heuristic (unvalidated against live upstream) |
| Model synonyms (`gemini-3.5-flash`, `gemini-3.6-flash`, `gemini-3.7-flash`, etc.) | Unverified compatibility alias table |

---

## Claim Verification Matrix

Statuses follow strict verification rules:
- **VERIFIED**: Reproduced with live or executed test evidence in this session.
- **PARTIALLY VERIFIED**: Some scenarios work; others fail or lack evidence.
- **IMPLEMENTED**: Source code and/or unit tests exist in repository, but execution and/or live upstream validation is missing (`BLOCKED` in this environment).
- **UNVERIFIED**: Insufficient evidence or unvalidated upstream behavior.
- **UNSUPPORTED**: Not implemented.
- **INCOMPATIBLE**: Confirmed protocol mismatch.

| Feature | Source Location | Test | Result | Evidence |
|---|---|---|---|---|
| OAuth Authorization Flow (Antigravity) | — | — | **UNSUPPORTED** | `9router-go` has no Antigravity `/authorize` or `/callback` route; relies on SQLite import (`internal/handlers/oauth/oauth.go`) or external dashboard |
| OAuth Token Refresh (Antigravity) | `internal/providers/oauth.go`, `internal/handlers/chat/gemini_handler.go` | `TestKnownOAuthConfigs_NoHardcodedGoogleSecrets`, `TestAntigravity_ConcurrentRefreshSerializedAndRotatesToken`, `TestLiveAntigravity_A_OAuthRefresh` | **IMPLEMENTED** (live **BLOCKED**) | Removed embedded Google client secret (`9b1d8aa`); added per-connection mutex and `refreshToken` rotation persistence; `go test` / live refresh not executed (`go` binary missing & no live creds) |
| Project ID Discovery (`loadCodeAssist` / `onboardUser`) | `internal/handlers/chat/antigravity_project.go`, `internal/handlers/chat/gemini_handler.go` | `TestFetchAntigravityProjectID_outcomes`, `TestAntigravity_NoProjectID_ReturnsExplicitError` | **IMPLEMENTED** (live **BLOCKED**) | Removed fabricated `cloudcode-pa-<hex>` fallback (`8724de2`); bounded `loadCodeAssist`/`onboardUser` bodies (`1 MiB`) and context-aware `sleepProbeDelay` |
| Request Envelope & Basic Generation | `internal/translator/antigravity.go`, `internal/translator/gemini.go`, `internal/proxy/gemini.go`, `internal/proxy/executor/gemini.go` | `TestTranslateOpenAIToGemini_CoalescesParallelToolResults`, `TestLiveAntigravity_B_BasicGeneration` | **IMPLEMENTED** (live **BLOCKED**) | Wraps Gemini JSON into `AntigravityRequest` (`v1internal:generateContent`), uses camelCase `systemInstruction`, coalesces parallel `role: "tool"` turns into one `user` turn, and unwraps `{"response": ...}` in both `proxy/gemini.go` and `executor/gemini.go` |
| SSE Streaming | `internal/handlers/chat/gemini_handler.go`, `internal/translator/gemini.go`, `internal/translator/response.go`, `internal/proxy/executor/gemini.go` | `TestGeminiStream_EmitsDoneAndUniqueToolIndices`, `TestTranslateGeminiChunkToOpenAI_FinishReasonToolCalls`, `TestLiveAntigravity_C_Streaming` | **IMPLEMENTED** (live **BLOCKED**) | Emits `data: [DONE]\n\n`, tracks `GeminiStreamState.HasToolCalls` -> `"finish_reason": "tool_calls"`, removes duplicate `\n\n`, unwraps non-SSE fallbacks, sorts Claude streaming `tool_use` closing indices deterministically; not yet executed (`go` binary missing & no live creds) |
| Tool Calling & `_ide` Cloaking | `internal/translator/antigravity.go`, `internal/translator/gemini.go`, `internal/translator/schema.go` | `TestCloakAntigravityRequest_RenamesAndInjectsDecoys`, `TestTranslateGeminiChunkToOpenAI_IncrementsToolCallIndices`, `TestTranslateOpenAIToClaudeStreamSession_DeterministicToolBlockOrder`, `TestLiveAntigravity_D_ToolCalling` | **IMPLEMENTED** (live **BLOCKED**) | Unique tool `index` + `finish_reason: "tool_calls"`, parallel tool response coalescing, deterministic Claude block closing order; 21 decoy tools use placeholder `{reason: string}` schema rather than real IDE schemas — live upstream acceptance unverified |
| 21 Decoy Tools Execution | `internal/translator/antigravity.go` | — | **UNSUPPORTED** | Declared only as `"This tool is currently unavailable."` decoy schemas; no tool execution exists in proxy |
| System Prompt Stripping | `internal/translator/antigravity.go` | `TestStripCompetitivePrompts` | **IMPLEMENTED** | Scoped `StripCompetitivePrompts` to `SystemInstruction` only so user `Contents` are never silently altered |
| Model Mapping & Limits | `internal/translator/antigravity.go`, `internal/providers/capabilities.go` | `TestNormalizeAntigravityModel`, `TestNormalizeAntigravityModel_AllSynonymsValid` | **UNVERIFIED** | Alias names (`gemini-3.5-flash`, `gemini-3.6-flash`, `gemini-3.7-flash`) and `1M / 64K` limits are static heuristics; live availability unverified (`BLOCKED`) |
| Multimodal (Image / Audio / PDF) | `internal/translator/gemini.go`, `internal/translator/request.go`, `internal/translator/antigravity.go` | `TestOpenAIToGemini_HttpImageUrl_FileData`, `TestOpenAIToGemini_InputAudio`, `TestTranslateClaudeToOpenAI_DocumentPDFBlock`, `TestLiveAntigravity_E_Multimodal` | **PARTIALLY VERIFIED** (code) / **INCOMPATIBLE** (HTTP `image_url` wildcard) | Base64 images, `input_audio`, and Claude `document` (PDF) blocks translate to `inlineData`; arbitrary HTTP `image_url` emits `fileData{mimeType: "image/*"}` which Google's Gemini API generally rejects unless it is a Google Files API URI |
| Image Generation (`image_gen`) | `internal/translator/antigravity.go`, `internal/proxy/gemini.go` | `TestWrapAntigravityImageRequest` | **IMPLEMENTED** (live **BLOCKED**) | Builds `requestType: "image_gen"` payload; only reads text prompt (ignores input image edits in `ForwardGemini`) |
| Proxy Pool Routing (HTTP/HTTPS/SOCKS5) | `internal/handlers/chat/connections.go`, `internal/db/proxyPools.go` | `TestGetClientForConnection_SupportsSOCKS5AndStrictProxyFailsClosed` | **IMPLEMENTED** | Fixed bypass where `forwardGeminiNativeRequest` used `h.Client` instead of connection `httpClient`; added `https`/`socks5` support and `strictProxy` fail-closed |
| MITM Interception (Antigravity) | `internal/mitm/handlers/antigravity.go`, `internal/mitm/dns.go` | `internal/mitm/mitm_test.go` | **IMPLEMENTED** (Windows **UNSUPPORTED**) | Linux/macOS `/etc/hosts` + local TLS; Windows explicitly returns unsupported error |
