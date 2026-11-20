package executor

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"9router/proxy/internal/log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"9router/proxy/internal/providers"
	"9router/proxy/internal/proxy"
	"9router/proxy/internal/shutdown"
	"9router/proxy/internal/translator"
)

// ---- Provider-specific executors ----

// ForwardGrokCLI forwards to grok-cli using Responses API format.
// Transforms Chat Completions body → Responses API body before forwarding.
func ForwardGrokCLI(w http.ResponseWriter, req *Request) error {
	transformedBody, _, err := buildResponsesBody(req.Body)
	if err != nil {
		return fmt.Errorf("transform body: %w", err)
	}
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := proxy.ForwardGrokCLI(ctx, req.Client, req.Config, req.APIKey, transformedBody, req.IsStream)
	if err != nil {
		return fmt.Errorf("ForwardGrokCLI: %w", err)
	}
	defer resp.Body.Close()
	stallReader := proxy.NewStallReader(resp.Body, 0, "grok-cli")
	defer stallReader.Close()
	return handleCodexStream(w, req, stallReader)
}

// ForwardCodex forwards to codex using Responses API format.
// Transforms Chat Completions body → Responses API body before forwarding.
func ForwardCodex(w http.ResponseWriter, req *Request) error {
	transformedBody, _, err := buildResponsesBody(req.Body)
	if err != nil {
		return fmt.Errorf("transform body: %w", err)
	}
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := proxy.ForwardCodex(ctx, req.Client, req.Config, req.APIKey, transformedBody, req.IsStream)
	if err != nil {
		return fmt.Errorf("ForwardCodex: %w", err)
	}
	defer resp.Body.Close()
	stallReader := proxy.NewStallReader(resp.Body, 0, "codex")
	defer stallReader.Close()
	return handleCodexStream(w, req, stallReader)
}

// ForwardIflow forwards to iflow with HMAC-SHA256 signature.
// Injects stream_options and generates HMAC headers before forwarding.
func ForwardIflow(w http.ResponseWriter, req *Request) error {
	var reqMap map[string]interface{}
	if err := json.Unmarshal(req.Body, &reqMap); err != nil {
		return fmt.Errorf("parse body: %w", err)
	}
	if req.IsStream {
		reqMap["stream"] = true
		if _, ok := reqMap["stream_options"]; !ok {
			reqMap["stream_options"] = map[string]interface{}{"include_usage": true}
		}
	}
	reqBody, err := json.Marshal(reqMap)
	if err != nil {
		return fmt.Errorf("marshal iflow body: %w", err)
	}

	// HMAC-SHA256 signature
	sessionID := "session-" + uuid.New().String()
	timestamp := time.Now().UnixMilli()
	userAgent := "iFlow-Cli"
	payload := userAgent + ":" + sessionID + ":" + strconv.FormatInt(timestamp, 10)

	mac := hmac.New(sha256.New, []byte(req.APIKey))
	mac.Write([]byte(payload))
	signature := hex.EncodeToString(mac.Sum(nil))

	extraHeaders := map[string]string{
		"User-Agent":       userAgent,
		"session-id":       sessionID,
		"x-iflow-timestamp": strconv.FormatInt(timestamp, 10),
		"x-iflow-signature": signature,
	}
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := proxy.ForwardIflow(ctx, req.Client, req.Config, req.APIKey, reqBody, req.IsStream, extraHeaders)
	if err != nil {
		return fmt.Errorf("ForwardIflow upstream: %w", err)
	}
	defer resp.Body.Close()

	if req.IsStream {
		stallReader := proxy.NewStallReader(resp.Body, 0, "iflow")
		defer stallReader.Close()
		return execSSEStream(w, stallReader, req)
	}
	return jsonResponse(req.Ctx, w, resp.Body, req.TranslateResp, req.ResponseBuf)
}

// ForwardKimchi forwards to kimchi with Anthropic field stripping.
// Cleans Anthropic-specific fields from body before forwarding.
func ForwardKimchi(w http.ResponseWriter, req *Request) error {
	var reqBody map[string]any
	if err := json.Unmarshal(req.Body, &reqBody); err != nil {
		return fmt.Errorf("parse request body: %w", err)
	}

	CleanKimchiBody(reqBody)

	cleanedBody, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshal cleaned body: %w", err)
	}

	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := proxy.ForwardKimchi(ctx, req.Client, req.Config, req.APIKey, cleanedBody, req.IsStream)
	if err != nil {
		return fmt.Errorf("ForwardKimchi: %w", err)
	}
	defer resp.Body.Close()

	if req.IsStream {
		stallReader := proxy.NewStallReader(resp.Body, 0, "kimchi")
		defer stallReader.Close()
		return execSSEStream(w, stallReader, req)
	}
	return jsonResponse(req.Ctx, w, resp.Body, req.TranslateResp, req.ResponseBuf)
}

// ForwardKiro forwards to kiro with AWS EventStream response handling.
// Uses EventStream binary parsing instead of standard SSE.
func ForwardKiro(w http.ResponseWriter, req *Request) error {
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := proxy.ForwardKiro(ctx, req.Client, req.Config, req.APIKey, req.Body, req.IsStream)
	if err != nil {
		return fmt.Errorf("ForwardKiro: %w", err)
	}
	defer resp.Body.Close()
	stallReader := proxy.NewStallReader(resp.Body, 0, "kiro")
	defer stallReader.Close()
	return handleKiroStream(w, req, stallReader)
}

// ForwardAzure forwards to Azure OpenAI with dynamic URL from env vars.
func ForwardAzure(w http.ResponseWriter, req *Request) error {
	var oreq struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(req.Body, &oreq); err != nil {
		log.Warn("executor", "azure unmarshal body", "error", err)
	}
	modelName := oreq.Model
	if modelName == "" {
		modelName = "gpt-4"
	}

	endpoint := os.Getenv("AZURE_ENDPOINT")
	apiVersion := os.Getenv("AZURE_API_VERSION")
	if apiVersion == "" {
		apiVersion = "2024-10-01-preview"
	}
	deployment := os.Getenv("AZURE_DEPLOYMENT")
	if deployment == "" {
		deployment = modelName
	}
	if endpoint == "" {
		endpoint = "https://api.openai.com"
	}

	baseURL := strings.TrimRight(endpoint, "/")
	url := fmt.Sprintf("%s/openai/deployments/%s/chat/completions?api-version=%s",
		baseURL, deployment, apiVersion)

	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	r, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(req.Body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("api-key", req.APIKey)
	if req.IsStream {
		r.Header.Set("Accept", "text/event-stream")
	}

	resp, err := req.Client.Do(r)
	if err != nil {
		return fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
		if readErr != nil {
			return &proxy.UpstreamError{StatusCode: resp.StatusCode, Body: []byte("failed to read error body")}
		}
		return &proxy.UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
	}

	if req.IsStream {
		stallReader := proxy.NewStallReader(resp.Body, 0, "azure")
		defer stallReader.Close()
		return execSSEStream(w, stallReader, req)
	}
	return jsonResponse(req.Ctx, w, resp.Body, req.TranslateResp, req.ResponseBuf)
}

// ForwardCommandcode forwards to CommandCode with NDJSON→SSE translation.
func ForwardCommandcode(w http.ResponseWriter, req *Request) error {
	var oreq struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(req.Body, &oreq); err != nil {
		log.Warn("executor", "commandcode unmarshal body", "error", err)
	}

	// Force stream=true — commandcode always uses NDJSON
	var reqMap map[string]interface{}
	if err := json.Unmarshal(req.Body, &reqMap); err != nil {
		return fmt.Errorf("parse body: %w", err)
	}
	reqMap["stream"] = true
	reqBody, err := json.Marshal(reqMap)
	if err != nil {
		return fmt.Errorf("marshal commandcode body: %w", err)
	}

	// Build request with custom headers (not using proxy.ForwardCommandcode)
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	r, err := http.NewRequestWithContext(ctx, "POST", req.Config.BaseURL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+req.APIKey)
	r.Header.Set("x-session-id", uuid.New().String())
	r.Header.Set("x-command-code-version", "0.25.7")
	r.Header.Set("x-cli-environment", "cli")
	r.Header.Set("Accept", "text/event-stream")

	resp, err := req.Client.Do(r)
	if err != nil {
		return fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1*1024*1024))
		if readErr != nil {
			return &proxy.UpstreamError{StatusCode: resp.StatusCode, Body: []byte("failed to read error body")}
		}
		return &proxy.UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
	}

	stallReader := proxy.NewStallReader(resp.Body, 0, "commandcode")
	defer stallReader.Close()
	return handleCommandcodeStream(w, req, stallReader, oreq.Model)
}

// ForwardOpencode handles requests for opencode (free tier).
func ForwardOpencode(w http.ResponseWriter, req *Request) error {
	apiKey := req.APIKey
	if apiKey == "" {
		apiKey = "public"
	}

	body := InjectReasoningContent(req.Body, "opencode")

	cfg := *req.Config
	cfg.StaticHeaders = proxy.BuildOpenCodeHeaders(cfg.StaticHeaders, req.SessionID, req.IsStream)

	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := proxy.ForwardOpenAI(ctx, req.Client, &cfg, apiKey, body, req.IsStream)
	if err != nil {
		return fmt.Errorf("ForwardOpencode: %w", err)
	}
	defer resp.Body.Close()

	if req.IsStream {
		stallReader := proxy.NewStallReader(resp.Body, 0, "opencode")
		defer stallReader.Close()
		return execSSEStream(w, stallReader, req)
	}
	return jsonResponse(req.Ctx, w, resp.Body, req.TranslateResp, req.ResponseBuf)
}

var opencodeGoMessagesModels = map[string]bool{
	"minimax-m3":   true,
	"minimax-m2.7": true,
	"minimax-m2.5": true,
	"qwen3.7-max":  true,
	"qwen3.7-plus": true,
	"qwen3.6-plus": true,
}

func opencodeGoMessagesURL(cfg *providers.ProviderConfig) string {
	const defaultURL = "https://opencode.ai/zen/go/v1/messages"
	if cfg == nil || cfg.BaseURL == "" || cfg.BaseURL == "https://opencode.ai/zen/go/v1/chat/completions" {
		return defaultURL
	}
	if strings.Contains(cfg.BaseURL, "/chat/completions") {
		return strings.Replace(cfg.BaseURL, "/chat/completions", "/messages", 1)
	}
	return strings.TrimRight(cfg.BaseURL, "/") + "/messages"
}

func execClaudeMessagesSSEStream(w http.ResponseWriter, upstream io.Reader, req *Request) error {
	flusher := proxy.WriteSSEHeaders(w)
	startTime := req.StartTime
	if startTime.IsZero() {
		startTime = time.Now()
	}
	var state translator.ClaudeToOpenAIStreamState
	err := proxy.ScanStream(upstream, func(chunk []byte) {
		trimmed := bytes.TrimSpace(chunk)
		if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
			return
		}
		openaiSSE, tErr := translator.TranslateClaudeStreamToOpenAI(trimmed, &state)
		if tErr != nil {
			log.Error("executor", "opencode-go claude stream translate error", "error", tErr)
		}
		var out []byte
		if req.TranslateResp {
			// Caller requested /v1/messages (Anthropic format); pass through upstream Anthropic SSE.
			var ev struct {
				Type string `json:"type"`
			}
			_ = json.Unmarshal(trimmed, &ev)
			if ev.Type != "" {
				out = []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", ev.Type, string(trimmed)))
			} else {
				out = []byte(fmt.Sprintf("data: %s\n\n", string(trimmed)))
			}
		} else {
			out = openaiSSE
		}
		if len(out) == 0 {
			return
		}
		if req.TTFT != nil && *req.TTFT == 0 {
			*req.TTFT = time.Since(startTime).Milliseconds()
		}
		if req.ResponseBuf != nil {
			req.ResponseBuf.Write(out)
		}
		w.Write(out)
		if flusher != nil {
			flusher.Flush()
		}
	})
	if !req.TranslateResp && !state.DoneSent && (err == nil || shutdown.Fired()) {
		w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	if state.Usage != nil && req.Ctx != nil {
		translator.SetUsage(req.Ctx, state.Usage)
	}
	return err
}

// ForwardOpencodeGo handles requests for opencode-go (paid tier).
func ForwardOpencodeGo(w http.ResponseWriter, req *Request) error {
	body := InjectReasoningContent(req.Body, "opencode-go")

	var reqObj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &reqObj); err != nil {
		log.Warn("executor", "opencode unmarshal body", "error", err)
	}

	if opencodeGoMessagesModels[reqObj.Model] {
		// Route to /zen/go/v1/messages (Anthropic/Claude format)
		messagesURL := opencodeGoMessagesURL(req.Config)
		claudeBody, tErr := translator.TranslateOpenAIToClaudeRequest(body)
		if tErr != nil {
			return fmt.Errorf("ForwardOpencodeGo translate request: %w", tErr)
		}
		headers := map[string]string{
			"Content-Type":      "application/json",
			"x-api-key":         req.APIKey,
			"anthropic-version": "2023-06-01",
		}
		if req.IsStream {
			headers["Accept"] = "text/event-stream"
		}
		ctx := req.Ctx
		if ctx == nil {
			ctx = context.Background()
		}
		resp, err := proxy.DoRequest(ctx, req.Client, "POST", messagesURL, headers, claudeBody)
		if err != nil {
			return fmt.Errorf("ForwardOpencodeGo (messages route): %w", err)
		}
		defer resp.Body.Close()

		if req.IsStream {
			stallReader := proxy.NewStallReader(resp.Body, 0, "opencode-go")
			defer stallReader.Close()
			return execClaudeMessagesSSEStream(w, stallReader, req)
		}
		if req.TranslateResp {
			return jsonResponse(req.Ctx, w, resp.Body, false, req.ResponseBuf)
		}
		rawResp, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
		if err != nil {
			return fmt.Errorf("ForwardOpencodeGo read messages response: %w", err)
		}
		if req.ResponseBuf != nil {
			req.ResponseBuf.Write(rawResp)
		}
		openaiResp, usage, tErr := translator.TranslateClaudeResponseToOpenAI(rawResp)
		if tErr != nil {
			return fmt.Errorf("ForwardOpencodeGo translate response: %w", tErr)
		}
		if usage != nil && req.Ctx != nil {
			translator.SetUsage(req.Ctx, usage)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(openaiResp)
		return nil
	}

	// Default OpenAI format endpoint: https://opencode.ai/zen/go/v1/chat/completions
	ctx := req.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := proxy.ForwardOpenAI(ctx, req.Client, req.Config, req.APIKey, body, req.IsStream)
	if err != nil {
		return fmt.Errorf("ForwardOpencodeGo (default route): %w", err)
	}
	defer resp.Body.Close()

	if req.IsStream {
		stallReader := proxy.NewStallReader(resp.Body, 0, "opencode-go")
		defer stallReader.Close()
		return execSSEStream(w, stallReader, req)
	}
	return jsonResponse(req.Ctx, w, resp.Body, req.TranslateResp, req.ResponseBuf)
}

