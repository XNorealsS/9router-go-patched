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

	"9router/proxy/internal/log"
	"9router/proxy/internal/providers"
	"9router/proxy/internal/proxy"
	"9router/proxy/internal/proxy/oauth"
	"9router/proxy/internal/shutdown"
	"9router/proxy/internal/translator"
)

const maxGeminiRespBytes = 10 * 1024 * 1024

// forwardGeminiNativeRequest handles forwarding for gemini-native providers (antigravity).
func (h *ChatHandler) forwardGeminiNativeRequest(
	ctx context.Context,
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
	return h.forwardGeminiNativeRequestWithClient(ctx, h.Client, w, provider, cfg, apiKey, connectionID, body, isStream, translateResponse, metrics)
}

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
	if connectionID == "" || h.Repo == nil {
		return currentToken, "", nil
	}

	mu := h.connRefreshLock(connectionID)
	mu.Lock()
	defer mu.Unlock()

	conn, err := h.Repo.GetProviderConnectionByID(connectionID)
	if err != nil || conn == nil {
		return currentToken, "", nil
	}
	provider := conn.Provider
	rawData := conn.Data

	var connMap map[string]interface{}
	if err := json.Unmarshal([]byte(rawData), &connMap); err != nil {
		return currentToken, "", nil
	}

	oauthData := providers.ParseOAuthConnection(connMap)
	projectID := ""
	if oauthData != nil {
		projectID = oauthData.ProjectID
	}
	if oauthData == nil || oauthData.RefreshToken == "" || !oauthData.IsExpired() {
		// Another goroutine may have refreshed while we waited for mu; return the fresh DB token if present.
		if oauthData != nil && oauthData.AccessToken != "" && !oauthData.IsExpired() {
			return oauthData.AccessToken, projectID, nil
		}
		return currentToken, projectID, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Try per-provider OAuth refresher first
	if refresher := oauth.Get(provider); refresher != nil {
		log.Info("oauth", "token expired, custom refresh", "provider", provider, "project", projectID)
		result, err := refresher(ctx, &oauth.Params{
			Client:       h.Client,
			Provider:     provider,
			RefreshToken: oauthData.RefreshToken,
			AccessToken:  currentToken,
		})
		if err != nil {
			return currentToken, projectID, fmt.Errorf("OAuth refresh for %s: %w", provider, err)
		}
		update := oauth.BuildConnectionUpdate(result)
		var existing map[string]interface{}
		if err := json.Unmarshal([]byte(rawData), &existing); err != nil {
			log.Error("oauth", "unmarshal connection data failed", "conn", connectionID, "error", err)
			existing = make(map[string]interface{})
		}
		for k, v := range update {
			existing[k] = v
		}
		if result.ProjectID != "" {
			projectID = result.ProjectID
			existing["projectId"] = projectID
		}
		mergedJSON, err := json.Marshal(existing)
		if err != nil {
			log.Error("oauth", "marshal connection data failed", "conn", connectionID, "error", err)
		} else if err := h.Repo.UpdateProviderConnectionData(connectionID, string(mergedJSON), time.Now().UTC().Format(time.RFC3339)); err != nil {
			log.Warn("oauth", "persist refreshed token failed", "conn", connectionID, "error", err)
		}
		log.Info("oauth", "token refreshed", "provider", provider, "project", projectID)
		return result.AccessToken, projectID, nil
	}

	// Fall back to standard OAuth2
	cfg, ok := providers.KnownOAuthConfigs[provider]
	if !ok {
		return currentToken, projectID, nil
	}

	log.Info("oauth", "token expired, standard refresh", "provider", provider, "project", projectID)
	tokenResp, err := providers.RefreshTokenWithContext(ctx, h.Client, cfg, oauthData.RefreshToken)
	if err != nil {
		return currentToken, projectID, fmt.Errorf("OAuth refresh for %s: %w", provider, err)
	}

	update := tokenResp.BuildConnectionUpdate()
	var existing map[string]interface{}
	if err := json.Unmarshal([]byte(rawData), &existing); err != nil {
		log.Error("oauth", "unmarshal connection data failed", "conn", connectionID, "error", err)
		existing = make(map[string]interface{})
	}
	for k, v := range update {
		existing[k] = v
	}
	mergedJSON, err := json.Marshal(existing)
	if err != nil {
		log.Error("oauth", "marshal connection data failed", "conn", connectionID, "error", err)
	} else if err := h.Repo.UpdateProviderConnectionData(connectionID, string(mergedJSON), time.Now().UTC().Format(time.RFC3339)); err != nil {
		log.Warn("oauth", "persist refreshed token failed", "conn", connectionID, "error", err)
	}

	log.Info("oauth", "token refreshed", "provider", provider, "project", projectID)
	return tokenResp.AccessToken, projectID, nil
}

// forceRefreshOAuthToken unconditionally refreshes the OAuth token for a connection ID.
func (h *ChatHandler) forceRefreshOAuthToken(connectionID string) (string, string, error) {
	if connectionID == "" || h.Repo == nil {
		return "", "", nil
	}

	mu := h.connRefreshLock(connectionID)
	mu.Lock()
	defer mu.Unlock()

	conn, err := h.Repo.GetProviderConnectionByID(connectionID)
	if err != nil {
		return "", "", err
	}
	if conn == nil {
		return "", "", fmt.Errorf("connection %s not found", connectionID)
	}
	provider := conn.Provider
	rawData := conn.Data

	var connMap map[string]interface{}
	if err := json.Unmarshal([]byte(rawData), &connMap); err != nil {
		return "", "", err
	}

	oauthData := providers.ParseOAuthConnection(connMap)
	if oauthData == nil || oauthData.RefreshToken == "" {
		return "", "", fmt.Errorf("no refresh token available")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Try per-provider OAuth refresher first
	if refresher := oauth.Get(provider); refresher != nil {
		log.Info("oauth", "force refresh", "provider", provider)
		result, err := refresher(ctx, &oauth.Params{
			Client:       h.Client,
			Provider:     provider,
			RefreshToken: oauthData.RefreshToken,
		})
		if err == nil && result != nil {
			var existing map[string]interface{}
			if err := json.Unmarshal([]byte(rawData), &existing); err != nil {
				log.Error("oauth", "unmarshal conn data failed", "conn", connectionID, "error", err)
				existing = make(map[string]interface{})
			}
			for k, v := range oauth.BuildConnectionUpdate(result) {
				existing[k] = v
			}
			pid := oauthData.ProjectID
			if result.ProjectID != "" {
				pid = result.ProjectID
				existing["projectId"] = pid
			}
			mergedJSON, err := json.Marshal(existing)
			if err != nil {
				log.Error("oauth", "marshal conn data failed", "conn", connectionID, "error", err)
			} else if err := h.Repo.UpdateProviderConnectionData(connectionID, string(mergedJSON), time.Now().UTC().Format(time.RFC3339)); err != nil {
				log.Warn("oauth", "persist force-refreshed token failed", "conn", connectionID, "error", err)
			}
			return result.AccessToken, pid, nil
		}
	}

	// Fall back to standard OAuth2
	cfg, ok := providers.KnownOAuthConfigs[provider]
	if !ok {
		return "", "", fmt.Errorf("no OAuth config for %s", provider)
	}

	log.Info("oauth", "force refresh (standard)", "provider", provider)
	tokenResp, err := providers.RefreshTokenWithContext(ctx, h.Client, cfg, oauthData.RefreshToken)
	if err != nil {
		return "", "", fmt.Errorf("OAuth refresh for %s: %w", provider, err)
	}

	update := tokenResp.BuildConnectionUpdate()
	var existing map[string]interface{}
	if err := json.Unmarshal([]byte(rawData), &existing); err != nil {
		log.Error("oauth", "unmarshal conn data failed", "conn", connectionID, "error", err)
		existing = make(map[string]interface{})
	}
	for k, v := range update {
		existing[k] = v
	}
	mergedJSON, err := json.Marshal(existing)
	if err != nil {
		log.Error("oauth", "marshal conn data failed", "conn", connectionID, "error", err)
	} else if err := h.Repo.UpdateProviderConnectionData(connectionID, string(mergedJSON), time.Now().UTC().Format(time.RFC3339)); err != nil {
		log.Warn("oauth", "persist force-refreshed token failed", "conn", connectionID, "error", err)
	}

	pid := oauthData.ProjectID
	if v, ok := existing["projectId"].(string); ok && v != "" {
		pid = v
	}
	return tokenResp.AccessToken, pid, nil
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
