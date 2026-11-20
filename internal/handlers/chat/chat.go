package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"9router/proxy/internal/autonomy"
	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/log"
	"9router/proxy/internal/providers"
	"9router/proxy/internal/translator"
	"9router/proxy/internal/updater"
)

// HandleChatCompletions handles POST /v1/chat/completions (OpenAI format requests).
func (h *ChatHandler) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	var reqBody struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &reqBody); err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if reqBody.Model == "" {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "missing model")
		return
	}

	// Bypass synthetic requests (Claude Code naming, warmup, keepalive)
	if handleBypassRequest(w, body, reqBody.Model, reqBody.Stream) {
		return
	}

	modelInfo, err := h.resolveModel(reqBody.Model)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	h.applySessionAffinity(r, body, modelInfo)
	h.recordAgentSessionTurn(r, body, modelInfo.Provider, modelInfo.ConnectionID, modelInfo.Model)

	if len(modelInfo.ComboModels) > 0 {
		if modelInfo.Strategy == "fusion" {
			h.handleFusion(r.Context(), w, body, modelInfo.ComboModels, modelInfo.Strategy, reqBody.Stream, false, reqBody.Model, modelInfo.StickyLimit)
			return
		}
		h.handleComboFallback(r.Context(), w, body, modelInfo.ComboModels, modelInfo.Strategy, reqBody.Stream, false, reqBody.Model, modelInfo.StickyLimit)
		return
	}

	h.handleSingleModel(r.Context(), w, body, modelInfo, reqBody.Stream, false)
}

// handleSingleModel resolves a single ModelInfo and forwards the request upstream.
func (h *ChatHandler) handleSingleModel(ctx context.Context, w http.ResponseWriter, body []byte, modelInfo *ModelInfo, isStream bool, translateResponse bool) {
	cw := newCommittedResponseWriter(w)
	var upstreamBody map[string]any
	if err := json.Unmarshal(body, &upstreamBody); err != nil {
		handlerutil.WriteJSONError(cw, http.StatusBadRequest, "failed to parse request body")
		return
	}
	upstreamBody["model"] = modelInfo.Model

	upstreamJSON, err := json.Marshal(upstreamBody)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "failed to marshal upstream request")
		return
	}

	result := h.handleAccountFallback(ctx, cw, modelInfo.Provider, modelInfo.Model, modelInfo.ConnectionID, upstreamJSON, isStream, translateResponse, "/v1/chat/completions")
	if result != nil {
		if cw.IsCommitted() {
			log.Error("chat", "upstream error after headers committed", "error", result)
			return
		}
		var ue *upstreamError
		if errors.As(result, &ue) {
			cw.Header().Set("Content-Type", "application/json")
			cw.WriteHeader(ue.StatusCode)
			cw.Write(ue.Body)
			return
		}
		handlerutil.WriteJSONError(cw, http.StatusBadGateway, fmt.Sprintf("upstream error: %v", result))
	}
}

// HandleMessages handles POST /v1/messages (Claude format requests).
func (h *ChatHandler) HandleMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Error("chat", "read body failed", "error", err)
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	var reqBody struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(body, &reqBody); err != nil {
		log.Error("chat", "parse JSON failed", "error", err)
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if reqBody.Model == "" {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "missing model")
		return
	}

	modelInfo, err := h.resolveModel(reqBody.Model)
	if err != nil {
		log.Error("chat", "resolve model failed", "error", err, "model", reqBody.Model)
		handlerutil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	h.applySessionAffinity(r, body, modelInfo)
	h.recordAgentSessionTurn(r, body, modelInfo.Provider, modelInfo.ConnectionID, modelInfo.Model)

	translateResponse := true
	var workingBody map[string]any
	if modelInfo.Provider == "claude" || modelInfo.Provider == "anthropic" {
		translateResponse = false
		if err := json.Unmarshal(body, &workingBody); err != nil {
			handlerutil.WriteJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
	} else {
		openaiBody, err := translator.TranslateClaudeToOpenAI(body)
		if err != nil {
			log.Error("chat", "translate failed", "error", err)
			handlerutil.WriteJSONError(w, http.StatusBadRequest, fmt.Sprintf("translation error: %v", err))
			return
		}
		if err := json.Unmarshal(openaiBody, &workingBody); err != nil {
			log.Error("chat", "parse translated failed", "error", err)
			handlerutil.WriteJSONError(w, http.StatusInternalServerError, "failed to parse translated request")
			return
		}
	}
	workingBody["stream"] = reqBody.Stream

	if len(modelInfo.ComboModels) > 0 {
		if modelInfo.Strategy == "fusion" {
			bodyJSON, err := json.Marshal(workingBody)
			if err != nil {
				handlerutil.WriteJSONError(w, http.StatusInternalServerError, "failed to marshal request body")
				return
			}
			h.handleFusion(r.Context(), w, bodyJSON, modelInfo.ComboModels, modelInfo.Strategy, reqBody.Stream, translateResponse, reqBody.Model, modelInfo.StickyLimit)
			return
		}
		h.handleMessagesComboFallback(r.Context(), w, workingBody, modelInfo.ComboModels, modelInfo.Strategy, reqBody.Stream, reqBody.Model, modelInfo.StickyLimit)
		return
	}

	h.handleMessagesSingleModel(r.Context(), w, workingBody, modelInfo, reqBody.Stream, translateResponse)
}

// handleMessagesSingleModel forwards a translated Claude request for a single model.
func (h *ChatHandler) handleMessagesSingleModel(ctx context.Context, w http.ResponseWriter, translatedReq map[string]any, modelInfo *ModelInfo, isStream bool, translateResponse bool) {
	cw := newCommittedResponseWriter(w)
	translatedReq["model"] = modelInfo.Model
	finalBody, err := json.Marshal(translatedReq)
	if err != nil {
		handlerutil.WriteJSONError(cw, http.StatusInternalServerError, "failed to marshal translated request")
		return
	}

	result := h.handleAccountFallback(ctx, cw, modelInfo.Provider, modelInfo.Model, modelInfo.ConnectionID, finalBody, isStream, translateResponse, "/v1/messages")
	if result != nil {
		if cw.IsCommitted() {
			log.Error("chat", "upstream error after headers committed", "error", result)
			return
		}
		var ue *upstreamError
		if errors.As(result, &ue) {
			cw.Header().Set("Content-Type", "application/json")
			cw.WriteHeader(ue.StatusCode)
			cw.Write(ue.Body)
			return
		}
		handlerutil.WriteJSONError(cw, http.StatusBadGateway, fmt.Sprintf("upstream error: %v", result))
	}
}

// HandleVersion responds with the proxy version details and update status.
func (h *ChatHandler) HandleVersion(w http.ResponseWriter, r *http.Request) {
	info := updater.GetCachedInfo()
	handlerutil.WriteJSON(w, http.StatusOK, info)
}

// HandleCheckUpdate fetches fresh update info from remote release server.
func (h *ChatHandler) HandleCheckUpdate(w http.ResponseWriter, r *http.Request) {
	info, err := updater.CheckUpdate(r.Context())
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadGateway, fmt.Sprintf("check update failed: %v", err))
		return
	}
	handlerutil.WriteJSON(w, http.StatusOK, info)
}

// HandleTriggerUpdate performs immediate self-updating if an update is available.
func (h *ChatHandler) HandleTriggerUpdate(w http.ResponseWriter, r *http.Request) {
	info, err := updater.CheckUpdate(r.Context())
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadGateway, fmt.Sprintf("check update failed: %v", err))
		return
	}
	if !info.HasUpdate {
		handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
			"status":  "up_to_date",
			"message": "9router-go is already on the latest version",
			"version": info.CurrentVersion,
		})
		return
	}
	if err := updater.PerformSelfUpdateVerified(info.DownloadURL, info.SHA256); err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, fmt.Sprintf("self-update failed: %v", err))
		return
	}
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"status":  "updated",
		"message": "Update installed successfully. Process is restarting...",
		"version": info.LatestVersion,
	})
}

// HandleModels responds with the list of available model identifiers from the DB.
func (h *ChatHandler) HandleModels(w http.ResponseWriter, r *http.Request) {
	type modelObj struct {
		ID                  string `json:"id"`
		Object              string `json:"object"`
		Created             int64  `json:"created"`
		OwnedBy             string `json:"owned_by"`
		ContextLength       int    `json:"context_length,omitempty"`
		MaxCompletionTokens int    `json:"max_completion_tokens,omitempty"`
	}

	var data []modelObj
	now := time.Now().Unix()

	aliases, err := h.Repo.GetModelAliases()
	if err == nil {
		for alias := range aliases {
			ctxLen, maxOut := providers.GetModelTokenLimits(alias)
			data = append(data, modelObj{
				ID:                  alias,
				Object:              "model",
				Created:             now,
				OwnedBy:             "system",
				ContextLength:       ctxLen,
				MaxCompletionTokens: maxOut,
			})
		}
	}

	combos, err := h.Repo.GetCombos()
	if err == nil {
		for _, c := range combos {
			ctxLen, maxOut := providers.GetModelTokenLimits(c.Name)
			data = append(data, modelObj{
				ID:                  c.Name,
				Object:              "model",
				Created:             now,
				OwnedBy:             "system",
				ContextLength:       ctxLen,
				MaxCompletionTokens: maxOut,
			})
		}
	}

	if data == nil {
		data = []modelObj{}
	}

	if h.Engine != nil {
		seen := make(map[string]bool, len(data))
		for _, d := range data {
			seen[d.ID] = true
		}
		for _, m := range h.Engine.Catalog.VerifiedAntigravityModels() {
			fullID := "antigravity/" + m.ID
			if seen[fullID] {
				continue
			}
			data = append(data, modelObj{
				ID:                  fullID,
				Object:              "model",
				Created:             now,
				OwnedBy:             "antigravity",
				ContextLength:       m.MaxTokens,
				MaxCompletionTokens: m.MaxOutputTokens,
			})
		}
	}

	if h.Repo != nil {
		if conns, cErr := h.Repo.GetProviderConnections("deepseek-web", true); cErr == nil && len(conns) > 0 {
			seen := make(map[string]bool, len(data))
			for _, d := range data {
				seen[d.ID] = true
			}
			for _, id := range []string{
				"deepseek-web/deepseek-chat",
				"deepseek-web/deepseek-reasoner",
				"deepseek-web/deepseek-v4-pro",
				"deepseek-web/deepseek-v4-pro-think",
				"deepseek-web/deepseek-v4-pro-search",
				"deepseek-web/deepseek-v4-flash",
			} {
				if seen[id] {
					continue
				}
				ctxLen, maxOut := providers.GetModelTokenLimits(id)
				data = append(data, modelObj{
					ID:                  id,
					Object:              "model",
					Created:             now,
					OwnedBy:             "deepseek-web",
					ContextLength:       ctxLen,
					MaxCompletionTokens: maxOut,
				})
			}
		}
	}

	{
		seen := make(map[string]bool, len(data))
		for _, d := range data {
			seen[d.ID] = true
		}
		for _, id := range []string{
			"kilo-gateway/kilo-auto/free",
			"kilo-gateway/nvidia/nemotron-3-ultra-550b-a55b:free",
			"kilo-gateway/nvidia/nemotron-3.5-super-vl:free",
			"kilo-gateway/nvidia/nemotron-3.5-lightning:free",
			"kilo-gateway/inclusionai/ling-3.2-flash:free",
			"kilo-gateway/baidu/ernie-5.5-flash-preview:free",
			"kilo-gateway/recursive/flint-v1.0:free",
			"kilo-gateway/liquid/lfm-3-1.7b:free",
		} {
			if seen[id] {
				continue
			}
			data = append(data, modelObj{
				ID:      id,
				Object:  "model",
				Created: now,
				OwnedBy: "kilo-gateway",
			})
		}
	}

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

func deriveRequestSessionKey(r *http.Request, body []byte, provider string) (string, int, int, bool) {
	var parsed struct {
		System   any `json:"system"`
		Messages []struct {
			Role      string `json:"role"`
			ToolCalls []any  `json:"tool_calls"`
			Content   any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", 0, 0, false
	}
	var toolCalls, toolResults int
	for _, m := range parsed.Messages {
		toolCalls += len(m.ToolCalls)
		if m.Role == "tool" {
			toolResults++
		}
		if parts, ok := m.Content.([]any); ok {
			for _, p := range parts {
				if pm, ok := p.(map[string]any); ok {
					switch pm["type"] {
					case "tool_use":
						toolCalls++
					case "tool_result":
						toolResults++
					}
				}
			}
		}
	}
	if toolCalls == 0 && toolResults == 0 && len(parsed.Messages) < 2 {
		return "", 0, 0, false
	}
	seed := fmt.Sprintf("%v", parsed.System)
	if len(seed) > 120 {
		seed = seed[:120]
	}
	if seed == "" && len(parsed.Messages) > 0 {
		b, _ := json.Marshal(parsed.Messages[0].Content)
		if len(b) > 120 {
			b = b[:120]
		}
		seed = string(b)
	}
	if xk := r.Header.Get("X-Session-Id"); xk != "" {
		seed = xk
	}
	bodyStr := string(body)
	hasThoughtSig := strings.Contains(bodyStr, "__ts__") || strings.Contains(bodyStr, "thoughtSignature") || strings.Contains(bodyStr, "thought_signature")
	return autonomy.DeriveSessionKey(r.RemoteAddr, provider, seed), toolCalls, toolResults, hasThoughtSig
}

func (h *ChatHandler) applySessionAffinity(r *http.Request, body []byte, info *ModelInfo) {
	if h.Engine == nil || h.Repo == nil || info == nil || info.ConnectionID != "" || len(info.ComboModels) > 0 {
		return
	}
	if detectNewTurn(body) {
		return
	}
	sessKey, _, _, _ := deriveRequestSessionKey(r, body, info.Provider)
	prevConn := h.Engine.Recorder.GetSessionConnection(sessKey)
	if prevConn == "" {
		return
	}
	if locked, err := h.Repo.IsConnectionModelLocked(prevConn, info.Model); err == nil && !locked {
		info.ConnectionID = prevConn
	}
}

func (h *ChatHandler) recordAgentSessionTurn(r *http.Request, body []byte, provider, connID, model string) {
	if h.Engine == nil {
		return
	}
	sessKey, toolCalls, toolResults, hasThoughtSig := deriveRequestSessionKey(r, body, provider)
	if sessKey == "" {
		return
	}
	h.Engine.Recorder.TrackSession(sessKey, provider, connID, model, toolCalls, toolResults, hasThoughtSig, "active")
}

// HandleModelsInfo returns metadata for a specific model.
// GET /v1/models/info?id={modelId}
func (h *ChatHandler) HandleModelsInfo(w http.ResponseWriter, r *http.Request) {
	modelID := r.URL.Query().Get("id")
	if modelID == "" {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "missing id query parameter")
		return
	}

	modelInfo, err := h.resolveModel(modelID)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusNotFound, fmt.Sprintf("model not found: %s", modelID))
		return
	}

	ctxLen, maxOut := providers.GetModelTokenLimits(modelID)

	info := map[string]any{
		"id":                    modelID,
		"object":                "model",
		"owned_by":              modelInfo.Provider,
		"endpoint":              "/v1/chat/completions",
		"context_length":        ctxLen,
		"max_completion_tokens": maxOut,
		"max_input_tokens":      ctxLen - maxOut,
		"max_output_tokens":     maxOut,
	}
	if len(modelInfo.ComboModels) > 0 {
		info["combo"] = true
		info["strategy"] = modelInfo.Strategy
		info["models"] = modelInfo.ComboModels
	}

	handlerutil.WriteJSON(w, http.StatusOK, info)
}

// HandleModelsByKind returns models filtered by service kind.
// GET /v1/models/{kind}
func (h *ChatHandler) HandleModelsByKind(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	if kind == "" {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "missing kind")
		return
	}

	var data []map[string]any
	now := time.Now().Unix()

	endpoint := "/v1/chat/completions"

	for id, cfg := range providers.KnownProviders {
		var match bool
		switch kind {
		case "image":
			match = cfg.ImageURL != ""
		case "tts":
			match = cfg.TTSURL != ""
		case "stt":
			match = cfg.STTURL != ""
		case "embedding":
			match = strings.Contains(cfg.BaseURL, "/embeddings")
		case "web":
			match = false
		case "image-to-text":
			match = true
		}
		if !match {
			continue
		}
		if kind == "web" {
			endpoint = "/v1/search"
		}
		if kind == "image" {
			endpoint = "/v1/images/generations"
		}
		if kind == "tts" {
			endpoint = "/v1/audio/speech"
		}
		if kind == "stt" {
			endpoint = "/v1/audio/transcriptions"
		}
		if kind == "embedding" {
			endpoint = "/v1/embeddings"
		}
		if kind == "image-to-text" {
			endpoint = "/v1/chat/completions"
		}

		data = append(data, map[string]any{
			"id":       id,
			"object":   "model",
			"kind":     kind,
			"owned_by": id,
			"endpoint": endpoint,
			"created":  now,
		})
	}

	if data == nil {
		data = []map[string]any{}
	}

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

// HandleCountTokens estimates Anthropic-format token count.
// POST /v1/messages/count_tokens
func (h *ChatHandler) HandleCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	inputTokens := estimateAnthropicTokens(body)
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"input_tokens": inputTokens,
	})
}

// estimateAnthropicTokens estimates input token count from Claude-format body.
// Matches JS estimateAnthropicInputTokens in count_tokens/route.js.
func estimateAnthropicTokens(body []byte) int {
	var msg struct {
		System any   `json:"system"`
		Tools  []any `json:"tools"`
	}
	if err := json.Unmarshal(body, &msg); err != nil {
		return 0
	}

	var totalChars int
	if sysStr, ok := msg.System.(string); ok {
		totalChars += len(sysStr)
	} else if sysArr, ok := msg.System.([]any); ok {
		for _, item := range sysArr {
			totalChars += countValueChars(item)
		}
	}

	var req struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err == nil {
		for _, m := range req.Messages {
			totalChars += messageContentChars(m.Content)
		}
	}

	var toolsReq struct {
		Tools []any `json:"tools"`
	}
	if err := json.Unmarshal(body, &toolsReq); err == nil {
		for _, t := range toolsReq.Tools {
			totalChars += countValueChars(t)
		}
	}

	if totalChars == 0 {
		totalChars = len(body)
	}
	return (totalChars + 3) / 4
}

// CountValueChars counts text characters in a generic JSON value.
func CountValueChars(v any) int {
	return countValueChars(v)
}

func countValueChars(v any) int {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case string:
		return len(val)
	case []byte:
		return len(val)
	case float64:
		return len(fmt.Sprintf("%v", val))
	case bool:
		if val {
			return 4
		}
		return 5
	case []any:
		n := 0
		for _, item := range val {
			n += countValueChars(item)
		}
		return n
	case map[string]any:
		n := 0
		for k, item := range val {
			n += len(k) + countValueChars(item)
		}
		return n
	}
	return 0
}

func messageContentChars(content any) int {
	if content == nil {
		return 0
	}
	switch c := content.(type) {
	case string:
		return len(c)
	case []any:
		n := 0
		for _, block := range c {
			n += contentBlockChars(block)
		}
		return n
	}
	return countValueChars(content)
}

func contentBlockChars(block any) int {
	if block == nil {
		return 0
	}
	m, ok := block.(map[string]any)
	if !ok {
		return countValueChars(block)
	}
	switch m["type"] {
	case "text":
		return countValueChars(m["text"])
	case "tool_use":
		return countValueChars(m["name"]) + countValueChars(m["input"])
	case "tool_result":
		return countValueChars(m["content"])
	case "thinking":
		return countValueChars(m["thinking"])
	default:
		return countValueChars(block)
	}
}

// HandleOllamaChat handles Ollama-compatible /v1/api/chat endpoint.
// POST /v1/api/chat
func (h *ChatHandler) HandleOllamaChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	defer r.Body.Close()

	// Ollama request format is close to OpenAI — forward to chat completions.
	newReq, _ := http.NewRequestWithContext(r.Context(), "POST", "/v1/chat/completions", bytes.NewReader(body))
	newReq.Header = r.Header
	h.HandleChatCompletions(w, newReq)
}
