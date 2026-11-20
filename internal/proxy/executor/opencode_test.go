package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"9router/proxy/internal/providers"
	"9router/proxy/internal/translator"
)

func TestInjectReasoningContent(t *testing.T) {
	input := []byte(`{
		"model": "deepseek-v4-flash-free",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "hi there"}
		]
	}`)

	res := InjectReasoningContent(input, "opencode")

	var reqMap map[string]any
	if err := json.Unmarshal(res, &reqMap); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}

	msgs := reqMap["messages"].([]any)
	asst := msgs[1].(map[string]any)
	if rc, ok := asst["reasoning_content"].(string); !ok || rc != " " {
		t.Errorf("expected reasoning_content to be ' ', got %v", asst["reasoning_content"])
	}
}

func TestForwardOpencode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer public" {
			t.Errorf("expected Bearer public, got %s", r.Header.Get("Authorization"))
		}
		if r.Header.Get("x-opencode-client") != "desktop" {
			t.Errorf("expected desktop client header, got %s", r.Header.Get("x-opencode-client"))
		}
		if r.Header.Get("x-opencode-project") != "global" {
			t.Errorf("expected x-opencode-project global, got %s", r.Header.Get("x-opencode-project"))
		}
		if !strings.HasPrefix(r.Header.Get("x-opencode-session"), "ses_") {
			t.Errorf("expected ses_ prefix on x-opencode-session, got %s", r.Header.Get("x-opencode-session"))
		}
		if !strings.HasPrefix(r.Header.Get("x-opencode-request"), "msg_") {
			t.Errorf("expected msg_ prefix on x-opencode-request, got %s", r.Header.Get("x-opencode-request"))
		}
		if r.Header.Get("User-Agent") != "opencode" {
			t.Errorf("expected User-Agent opencode, got %s", r.Header.Get("User-Agent"))
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"reasoning_content":" "`) {
			t.Errorf("expected reasoning_content injected in request body, got %s", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg_123","choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	cfg := &providers.ProviderConfig{
		BaseURL: srv.URL,
	}

	rec := httptest.NewRecorder()
	req := &Request{
		Client:        srv.Client(),
		Config:        cfg,
		APIKey:        "", // Should fallback to "public"
		Body:          []byte(`{"model":"deepseek-v4-flash-free","messages":[{"role":"assistant","content":"prev"}]}`),
		IsStream:      false,
		TranslateResp: false,
	}

	err := ForwardOpencode(rec, req)
	if err != nil {
		t.Fatalf("ForwardOpencode failed: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestForwardOpencodeGo_OpenAIRouting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret-go-key" {
			t.Errorf("expected Authorization Bearer secret-go-key header, got %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"msg_openai","choices":[{"message":{"content":"hello"}}]}`))
	}))
	defer srv.Close()

	cfg := &providers.ProviderConfig{
		BaseURL: srv.URL,
	}

	rec := httptest.NewRecorder()
	req := &Request{
		Client:        srv.Client(),
		Config:        cfg,
		APIKey:        "secret-go-key",
		Body:          []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`),
		IsStream:      false,
		TranslateResp: false,
	}

	err := ForwardOpencodeGo(rec, req)
	if err != nil {
		t.Fatalf("ForwardOpencodeGo failed: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}
}

func TestForwardOpencodeGo_MessagesTranslationNonStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/messages") {
			t.Errorf("expected /messages path, got %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "secret-go-key" {
			t.Errorf("expected x-api-key header, got %s", r.Header.Get("x-api-key"))
		}
		raw, _ := io.ReadAll(r.Body)
		var claudeReq map[string]any
		if err := json.Unmarshal(raw, &claudeReq); err != nil {
			t.Fatalf("expected valid JSON body: %v", err)
		}
		if claudeReq["system"] != "Be concise" {
			t.Errorf("expected translated system field, got %v", claudeReq["system"])
		}
		if mt, ok := claudeReq["max_tokens"].(float64); !ok || mt <= 0 {
			t.Errorf("expected positive max_tokens in Claude request, got %v", claudeReq["max_tokens"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_go_1","type":"message","role":"assistant","model":"minimax-m3","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":12,"output_tokens":4}}`))
	}))
	defer srv.Close()

	ctx := translator.WithUsageCapture(context.Background())
	rec := httptest.NewRecorder()
	err := ForwardOpencodeGo(rec, &Request{
		Ctx:           ctx,
		Client:        srv.Client(),
		Config:        &providers.ProviderConfig{BaseURL: srv.URL + "/v1/chat/completions"},
		APIKey:        "secret-go-key",
		Body:          []byte(`{"model":"minimax-m3","messages":[{"role":"system","content":"Be concise"},{"role":"user","content":"ping"}]}`),
		IsStream:      false,
		TranslateResp: false,
	})
	if err != nil {
		t.Fatalf("ForwardOpencodeGo failed: %v", err)
	}
	if !strings.Contains(rec.Body.String(), `"object":"chat.completion"`) || !strings.Contains(rec.Body.String(), `"content":"pong"`) {
		t.Fatalf("expected translated OpenAI completion response, got %s", rec.Body.String())
	}
	u := translator.GetAndClearUsage(ctx)
	if u == nil || u.PromptTokens != 12 || u.CompletionTokens != 4 {
		t.Fatalf("expected captured usage 12/4, got %+v", u)
	}
}

func TestForwardOpencodeGo_MessagesTranslationStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		events := []string{
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s1\",\"model\":\"qwen3.7-max\",\"usage\":{\"input_tokens\":18}}}\n\n",
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"stream ok\"}}\n\n",
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n",
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		}
		for _, e := range events {
			_, _ = w.Write([]byte(e))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer srv.Close()

	ctx := translator.WithUsageCapture(context.Background())
	rec := httptest.NewRecorder()
	err := ForwardOpencodeGo(rec, &Request{
		Ctx:           ctx,
		Client:        srv.Client(),
		Config:        &providers.ProviderConfig{BaseURL: srv.URL + "/v1/chat/completions"},
		APIKey:        "secret-go-key",
		Body:          []byte(`{"model":"qwen3.7-max","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		IsStream:      true,
		TranslateResp: false,
	})
	if err != nil {
		t.Fatalf("ForwardOpencodeGo stream failed: %v", err)
	}
	out := rec.Body.String()
	if !strings.Contains(out, `"content":"stream ok"`) || !strings.Contains(out, "data: [DONE]\n\n") {
		t.Fatalf("expected OpenAI SSE output + [DONE], got:\n%s", out)
	}
	u := translator.GetAndClearUsage(ctx)
	if u == nil || u.PromptTokens != 18 || u.CompletionTokens != 7 {
		t.Fatalf("expected captured stream usage 18/7, got %+v", u)
	}
}

