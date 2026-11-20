package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"9router/proxy/internal/autonomy"
	"9router/proxy/internal/log"
	"9router/proxy/internal/middleware"
	"9router/proxy/internal/providers"
	"9router/proxy/internal/proxy"
	"9router/proxy/internal/proxy/oauth"
	"9router/proxy/internal/shutdown"
	"9router/proxy/internal/translator"
)

const maxGeminiRespBytes = 10 * 1024 * 1024

func (h *ChatHandler) forwardGeminiNativeRequestWithClient(
	ctx context.Context,
	client *http.Client,
	w http.ResponseWriter,
	provider string,
	cfg *providers.ProviderConfig,
	apiKey string,
	connectionID string,
	body []byte,
	isStream bool,
	translateResponse bool,
	metrics *streamMetrics,
) error {
	if client == nil {
		client = h.Client
	}
	// Extract model
	var reqMeta struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &reqMeta); err != nil {
		log.Warn("gemini", "parse model failed", "error", err)
	}
	modelName := reqMeta.Model
	if modelName == "" {
		modelName = "gemini-3-flash"
	}

	// Read projectID (and refresh token if expired) for Gemini-native providers.
	var projectID string
	refreshedKey, pid, err := h.refreshOAuthTokenIfExpired(connectionID, apiKey)
	if err != nil {
		log.Warn("gemini", "token refresh error", "conn", connectionID, "error", err)
	} else {
		apiKey = refreshedKey
		projectID = pid
	}

	if (provider == "antigravity" || provider == "gemini-cli") && projectID == "" && !projectProbeCached(connectionID) {
		pid, authFailed, noProject := fetchAntigravityProjectID(ctx, client, apiKey)
		switch {
		case pid != "":
			projectID = pid
			h.storeAntigravityProjectID(connectionID, pid)
		case authFailed:
			// Upstream rejected the token (401/403) — refresh once and retry.
			// A missing/empty project is NOT an auth failure; refreshing there
			// only hammers the onboarding RPCs (the 429 source) without helping.
			log.Info("gemini", "force refresh OAuth token", "conn", connectionID)
			refreshedKey, pid2, err2 := h.forceRefreshOAuthToken(connectionID)
			if err2 == nil && refreshedKey != "" {
				apiKey = refreshedKey
				if pid2 != "" {
					projectID = pid2
					h.storeAntigravityProjectID(connectionID, pid2)
				} else if pid2, _, _ := fetchAntigravityProjectID(ctx, client, apiKey); pid2 != "" {
					projectID = pid2
					h.storeAntigravityProjectID(connectionID, pid2)
				}
			}
		case noProject:
			// Google definitively said this token has no project. Cache it so
			// client retries stop re-probing the onboarding RPCs.
			cacheProjectMissing(connectionID)
		}
	}

	if projectID == "" {
		if provider == "antigravity" {
			return fmt.Errorf("antigravity: no project ID — onboard the account in Antigravity (antigravity.google) or set projectId in connection data")
		}
		log.Info("gemini", "no projectID, fallback to OpenAI", "provider", provider)
		return h.forwardRequestWithClient(ctx, client, w, cfg, apiKey, body, isStream, translateResponse, metrics)
	}

	if provider == "antigravity" && h.Engine != nil {
		if healthyModel, changed, reason := h.Engine.Catalog.ResolveHealthyModel(modelName); changed {
			log.Info("autonomy", "rerouting deprecated model", "from", modelName, "to", healthyModel, "reason", reason)
			h.Engine.Recorder.Record(autonomy.Event{
				Category:     autonomy.CategoryRouting,
				Provider:     provider,
				ConnectionID: connectionID,
				Model:        modelName,
				Detected:     "requested model/alias is deprecated",
				Action:       "auto-routed to verified " + healthyModel,
				Reason:       reason,
				Outcome:      autonomy.OutcomeOK,
			})
			modelName = healthyModel
		}
		if q := h.Engine.Catalog.QuotaRemaining(modelName); q == 0 {
			h.Engine.Recorder.Record(autonomy.Event{
				Category:     autonomy.CategoryRouting,
				Provider:     provider,
				ConnectionID: connectionID,
				Model:        modelName,
				Detected:     "upstream catalog reports 0% quota remaining for model",
				Action:       "attempting request with backoff/failover readiness",
				Reason:       "quotaInfo.remainingFraction == 0",
				Outcome:      autonomy.OutcomeDegraded,
			})
		}
	}

	resp, err := proxy.ForwardGemini(ctx, client, cfg, apiKey, string(body), isStream, projectID, modelName)
	if err != nil {
		return fmt.Errorf("ForwardGemini (%s/%s): %w", provider, modelName, err)
	}

	var bodyCloser io.Closer = resp.Body
	defer func() {
		if bodyCloser != nil {
			bodyCloser.Close()
		}
	}()

	// Handle response — antigravity wraps non-streaming response in {"response": {...}}
	// For streaming (SSE), the events are NOT wrapped — pipe directly.
	if projectID != "" && !isStream {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxGeminiRespBytes))
		if err != nil {
			log.Error("gemini", "read response body failed", "error", err)
			return fmt.Errorf("read gemini response body: %w", err)
		}
		unwrapped := translator.UnwrapAntigravityResponse(raw)
		return h.handleGeminiNonStream(ctx, w, bytes.NewReader(unwrapped), translateResponse, metrics)
	}
	if isStream {
		contentType := resp.Header.Get("Content-Type")
		if !strings.HasPrefix(strings.ToLower(contentType), "text/event-stream") {
			raw, err := io.ReadAll(io.LimitReader(resp.Body, maxGeminiRespBytes))
			if err != nil {
				return fmt.Errorf("read gemini non-sse response body: %w", err)
			}
			if projectID != "" {
				raw = translator.UnwrapAntigravityResponse(raw)
			}
			return h.handleGeminiNonStream(ctx, w, bytes.NewReader(raw), translateResponse, metrics)
		}
		stallReader := proxy.NewStallReader(resp.Body, 0, provider)
		bodyCloser = stallReader
		return h.handleGeminiStream(ctx, w, stallReader, translateResponse, metrics)
	}
	return h.handleGeminiNonStream(ctx, w, resp.Body, translateResponse, metrics)
}

func (h *ChatHandler) connRefreshLock(connectionID string) *sync.Mutex {
	h.refreshLocksMu.Lock()
	defer h.refreshLocksMu.Unlock()
	if h.refreshLocks == nil {
		h.refreshLocks = make(map[string]*sync.Mutex)
	}
	mu, ok := h.refreshLocks[connectionID]
	if !ok {
		mu = &sync.Mutex{}
		h.refreshLocks[connectionID] = mu
	}
	return mu
}

// storeAntigravityProjectID persists a discovered Antigravity project ID on the
// connection so later requests skip the onboarding RPCs entirely.
func (h *ChatHandler) storeAntigravityProjectID(connectionID, pid string) {
	if connectionID == "" || pid == "" {
		return
	}
	if _, err := h.Repo.RawDB().Exec("UPDATE providerConnections SET data = json_set(data, '$.projectId', ?) WHERE id = ?", pid, connectionID); err != nil {
		log.Warn("gemini", "update projectId failed", "conn", connectionID, "error", err)
	}
}

// refreshOAuthTokenIfExpired checks and refreshes OAuth token for any provider with refreshToken.
// Uses a per-connection mutex to prevent concurrent refresh races and preserves rotated refresh tokens.
func (h *ChatHandler) refreshOAuthTokenIfExpired(connectionID, currentToken string) (string, string, error) {
	return h.refreshConnectionOAuth(connectionID, currentToken, false)
}

// forceRefreshOAuthToken unconditionally refreshes the OAuth token for a connection ID.
func (h *ChatHandler) forceRefreshOAuthToken(connectionID string) (string, string, error) {
	return h.refreshConnectionOAuth(connectionID, "", true)
}

func (h *ChatHandler) refreshConnectionOAuth(connectionID, currentToken string, force bool) (string, string, error) {
	if connectionID == "" || h.Repo == nil {
		return currentToken, "", nil
	}

	mu := h.connRefreshLock(connectionID)
	mu.Lock()
	defer mu.Unlock()

	conn, err := h.Repo.GetProviderConnectionByID(connectionID)
	if err != nil {
		if force {
			return "", "", err
		}
		return currentToken, "", nil
	}
	if conn == nil {
		if force {
			return "", "", fmt.Errorf("connection %s not found", connectionID)
		}
		return currentToken, "", nil
	}
	provider := conn.Provider
	rawData := conn.Data

	var connMap map[string]interface{}
	if err := json.Unmarshal([]byte(rawData), &connMap); err != nil {
		if force {
			return "", "", err
		}
		return currentToken, "", nil
	}

	oauthData := providers.ParseOAuthConnection(connMap)
	projectID := ""
	if oauthData != nil {
		projectID = oauthData.ProjectID
	}
	if oauthData == nil || oauthData.RefreshToken == "" {
		if force {
			return "", "", fmt.Errorf("no refresh token available")
		}
		return currentToken, projectID, nil
	}
	if !force && !oauthData.IsExpired() {
		// Another goroutine may have refreshed while we waited for mu; return the fresh DB token if present.
		if oauthData.AccessToken != "" {
			return oauthData.AccessToken, projectID, nil
		}
		return currentToken, projectID, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	persistUpdate := func(update map[string]interface{}, extraProjectID string) string {
		var existing map[string]interface{}
		if err := json.Unmarshal([]byte(rawData), &existing); err != nil {
			log.Error("oauth", "unmarshal conn data failed", "conn", connectionID, "error", err)
			existing = make(map[string]interface{})
		}
		for k, v := range update {
			existing[k] = v
		}
		pid := projectID
		if extraProjectID != "" {
			pid = extraProjectID
			existing["projectId"] = pid
		} else if v, ok := existing["projectId"].(string); ok && v != "" {
			pid = v
		}
		mergedJSON, err := json.Marshal(existing)
		if err != nil {
			log.Error("oauth", "marshal conn data failed", "conn", connectionID, "error", err)
		} else if err := h.Repo.UpdateProviderConnectionData(connectionID, string(mergedJSON), time.Now().UTC().Format(time.RFC3339)); err != nil {
			log.Warn("oauth", "persist refreshed token failed", "conn", connectionID, "error", err)
		}
		return pid
	}

	// 1. Try per-provider OAuth refresher first (requires registered refresher / env creds)
	if refresher := oauth.Get(provider); refresher != nil {
		log.Info("oauth", "refreshing token (custom)", "provider", provider, "project", projectID, "force", force)
		result, err := refresher(ctx, &oauth.Params{
			Client:       h.Client,
			Provider:     provider,
			RefreshToken: oauthData.RefreshToken,
			AccessToken:  currentToken,
		})
		if err == nil && result != nil {
			pid := persistUpdate(oauth.BuildConnectionUpdate(result), result.ProjectID)
			log.Info("oauth", "token refreshed", "provider", provider, "project", pid)
			return result.AccessToken, pid, nil
		}
		if err != nil && !force {
			return currentToken, projectID, fmt.Errorf("OAuth refresh for %s: %w", provider, err)
		}
	}

	// 2. Try standard OAuth2 config when operator provided ClientID
	cfg, ok := providers.KnownOAuthConfigs[provider]
	if ok && cfg.ClientID != "" {
		log.Info("oauth", "refreshing token (standard)", "provider", provider, "project", projectID, "force", force)
		tokenResp, err := providers.RefreshTokenWithContext(ctx, h.Client, cfg, oauthData.RefreshToken)
		if err != nil {
			return currentToken, projectID, fmt.Errorf("OAuth refresh for %s: %w", provider, err)
		}
		pid := persistUpdate(tokenResp.BuildConnectionUpdate(), "")
		log.Info("oauth", "token refreshed", "provider", provider, "project", pid)
		return tokenResp.AccessToken, pid, nil
	}

	// 3. Unified 1-Gateway bridge: ask companion dashboard over loopback to refresh into shared SQLite
	if h.DashboardURL != "" {
		if tok, pid, bErr := h.refreshViaDashboardBridge(ctx, connectionID); bErr == nil && tok != "" {
			log.Info("oauth", "token refreshed via 1-gateway dashboard bridge", "provider", provider, "conn", connectionID)
			return tok, pid, nil
		} else if bErr != nil {
			return currentToken, projectID, fmt.Errorf("OAuth refresh for %s via dashboard bridge: %w", provider, bErr)
		}
	}

	if !ok {
		if force {
			return "", "", fmt.Errorf("no OAuth config for %s", provider)
		}
		return currentToken, projectID, nil
	}
	return currentToken, projectID, fmt.Errorf("OAuth client credentials not configured (set provider OAUTH_CLIENT_ID/SECRET env vars)")
}

// refreshViaDashboardBridge asks the companion Next.js dashboard on loopback to run its
// provider OAuth refresh (/api/providers/<id>/test) and re-reads the fresh token from SQLite.
func (h *ChatHandler) refreshViaDashboardBridge(ctx context.Context, connectionID string) (string, string, error) {
	tokens := middleware.MintLoopbackAuthTokens(h.DataDir)
	if len(tokens) == 0 {
		tokens = []string{""}
	}
	client := &http.Client{Timeout: 15 * time.Second}
	var resp *http.Response
	for _, tok := range tokens {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.DashboardURL+"/api/providers/"+connectionID+"/test", nil)
		if err != nil {
			return "", "", err
		}
		if tok != "" {
			req.AddCookie(&http.Cookie{Name: "auth_token", Value: tok})
		}
		r, err := client.Do(req)
		if err != nil {
			return "", "", err
		}
		if r.StatusCode == http.StatusUnauthorized {
			_ = r.Body.Close()
			continue
		}
		resp = r
		break
	}
	if resp == nil {
		return "", "", fmt.Errorf("dashboard bridge returned HTTP 401")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("dashboard bridge returned HTTP %d", resp.StatusCode)
	}
	var out struct {
		Valid bool   `json:"valid"`
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out)

	updated, err := h.Repo.GetProviderConnectionByID(connectionID)
	if err != nil || updated == nil {
		if out.Error != "" {
			return "", "", fmt.Errorf("%s", out.Error)
		}
		return "", "", fmt.Errorf("failed to reload refreshed connection")
	}
	var updatedMap map[string]interface{}
	if err := json.Unmarshal([]byte(updated.Data), &updatedMap); err != nil {
		return "", "", err
	}
	oauthData := providers.ParseOAuthConnection(updatedMap)
	if oauthData == nil || oauthData.AccessToken == "" {
		return "", "", fmt.Errorf("empty access token after dashboard refresh")
	}
	if !out.Valid && out.Error != "" && oauthData.IsExpired() {
		return "", "", fmt.Errorf("%s", out.Error)
	}
	return oauthData.AccessToken, oauthData.ProjectID, nil
}

// handleGeminiStream processes Gemini stream SSE chunks and translates to OpenAI format.
func (h *ChatHandler) handleGeminiStream(ctx context.Context, w http.ResponseWriter, upstream io.Reader, translateResponse bool, metrics *streamMetrics) error {
	flusher := proxy.WriteSSEHeaders(w)
	geminiState := &translator.GeminiStreamState{}
	start := time.Now()
	// One session per stream so the OpenAI→Claude translation state cannot
	// collide across concurrent requests; always cleared on exit.
	sessionKey := fmt.Sprintf("gemini-stream-%d", time.Now().UnixNano())
	defer translator.ClearStreamState(sessionKey)
	finished := false

	err := proxy.ScanStream(upstream, func(chunk []byte) {
		chunkStr := strings.TrimSpace(string(chunk))
		if chunkStr == "" || chunkStr == "[DONE]" {
			return
		}

		// Unwrap antigravity envelope if present (SSE chunks may be wrapped as {"response": {...}})
		unwrapped := translator.UnwrapAntigravityResponse(chunk)

		// Translate each Gemini SSE chunk to OpenAI SSE chunk
		openaiChunk, err := translator.TranslateGeminiChunkToOpenAI(unwrapped, geminiState)
		if err != nil {
			log.Error("gemini", "chunk translate error", "error", err)
			return
		}
		if openaiChunk == nil {
			return
		}

		if metrics.TTFT == 0 {
			metrics.TTFT = time.Since(start).Milliseconds()
		}
		metrics.ResponseBuf.Write(openaiChunk)

		if translateResponse {
			// openaiChunk may have multiple SSE lines -- split and translate each
			for _, sse := range strings.Split(string(openaiChunk), "\n") {
				sse = strings.TrimSpace(sse)
				if !strings.HasPrefix(sse, "data: ") {
					continue
				}
				claudeChunk, tErr := translator.TranslateOpenAIToClaudeStreamSession(sessionKey, []byte(strings.TrimPrefix(sse, "data: ")))
				if tErr != nil {
					log.Error("gemini", "claude translate error", "error", tErr)
					continue
				}
				if claudeChunk == nil {
					continue
				}
				if bytes.Contains(claudeChunk, []byte("[DONE]")) {
					finished = true
				}
				w.Write(claudeChunk)
			}
		} else {
			// TranslateGeminiChunkToOpenAI already terminates each SSE event with \n\n.
			w.Write(openaiChunk)
		}
		if flusher != nil {
			flusher.Flush()
		}
	})

	if !translateResponse {
		w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	} else if shutdown.Fired() && !finished {
		w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}

	// Pull actual accumulated usage (incl. cached tokens) out of the session so
	// the log sees real numbers instead of the fallback estimate.
	if translateResponse {
		if usage := translator.GetStreamUsage(sessionKey); usage != nil {
			translator.SetUsage(ctx, usage)
		}
	} else if geminiState.Usage != nil {
		translator.SetUsage(ctx, geminiState.Usage)
	}
	return err
}

// handleGeminiNonStream translates a Gemini non-stream response to OpenAI format,
// and then to Claude format if translateResponse is true.
func (h *ChatHandler) handleGeminiNonStream(ctx context.Context, w http.ResponseWriter, upstream io.Reader, translateResp bool, metrics *streamMetrics) error {
	body, err := io.ReadAll(io.LimitReader(upstream, maxGeminiRespBytes))
	if err != nil {
		return fmt.Errorf("read gemini response body: %w", err)
	}

	if metrics != nil {
		metrics.ResponseBuf.Write(body)
	}

	// Use the full translator which handles tool calls, thinking, etc.
	openaiResp, usage, err := translator.TranslateGeminiResponseToOpenAI(body)
	if err == nil && usage != nil {
		translator.SetUsage(ctx, usage)
	}
	if err != nil {
		// Fallback: write raw body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(body)
		return nil
	}

	if translateResp {
		// OpenAI → Claude (Anthropic) format
		claudeResp, claudeUsage, tErr := translator.TranslateOpenAIToClaude(openaiResp)
		if tErr == nil && claudeUsage != nil {
			translator.SetUsage(ctx, claudeUsage)
		}
		if tErr != nil || claudeResp == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(openaiResp)
			return nil
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(claudeResp)
		return nil
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(openaiResp)
	return nil
}
