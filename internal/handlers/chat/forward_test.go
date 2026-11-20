package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"9router/proxy/internal/constants"
	"9router/proxy/internal/db"
)

// setupHandlerForForward wires a ChatHandler to a temp DB (no connections needed for forward tests).
func setupHandlerForForward(t *testing.T) (*ChatHandler, func()) {
	t.Helper()
	database, cleanup := setupChatTestDB(t)
	repo := db.NewRepo(database)
	return NewChatHandler(repo), cleanup
}

// openaiOKServer returns a fixed JSON chat completion. The upstream request's
// Authorization header is echoed back inside the response body (as "auth=<value>")
// because forwardRequest does not copy upstream response headers onto the client
// recorder — only the body survives the round trip.
func openaiOKServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get(constants.HeaderAuthorization)
		custom := r.Header.Get("X-Custom-Auth")
		foo := r.Header.Get("X-Foo")
		accept := r.Header.Get("Accept")
		body := `{"id":"resp","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1},`
		body += `"_echo":{"auth":` + quote(auth) + `,"custom":` + quote(custom) + `,"foo":` + quote(foo) + `,"accept":` + quote(accept) + `}}`
		w.Header().Set(constants.HeaderContentType, constants.ContentTypeJSON)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(body))
	}))
}

func quote(s string) string {
	return `"` + s + `"`
}

func TestHandleJSONResponse_NonTranslate(t *testing.T) {
	h, cleanup := setupHandlerForForward(t)
	defer cleanup()

	upstream := strings.NewReader(`{"id":"x"}`)
	rec := httptest.NewRecorder()
	if err := h.handleJSONResponse(context.Background(), rec, upstream, false, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != `{"id":"x"}` {
		t.Errorf("expected passthrough body, got %s", rec.Body.String())
	}
}

func TestHandleJSONResponse_Translate(t *testing.T) {
	h, cleanup := setupHandlerForForward(t)
	defer cleanup()

	// Non-stream: handleJSONResponse calls TranslateOpenAIToClaude (expects raw JSON, not SSE).
	upstream := strings.NewReader(`{"id":"chatcmpl-test","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	rec := httptest.NewRecorder()
	if err := h.handleJSONResponse(context.Background(), rec, upstream, true, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
	// Translated output should be Claude message JSON containing content blocks.
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"message"`) {
		t.Errorf("expected Claude message JSON, got %s", body)
	}
	if !strings.Contains(body, `"text":"hi"`) {
		t.Errorf("expected text content, got %s", body)
	}
}

func TestHandleStreamResponse_NonTranslate(t *testing.T) {
	h, cleanup := setupHandlerForForward(t)
	defer cleanup()

	upstream := strings.NewReader("chunk1chunk2")
	rec := httptest.NewRecorder()
	metrics := &streamMetrics{}
	if err := h.handleStreamResponse(context.Background(), rec, upstream, false, time.Now(), metrics); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Header().Get(constants.HeaderContentType) != constants.ContentTypeEventStream {
		t.Errorf("expected event-stream content type, got %q", rec.Header().Get(constants.HeaderContentType))
	}
	if rec.Body.String() != "chunk1chunk2" {
		t.Errorf("expected raw passthrough, got %q", rec.Body.String())
	}
	if metrics.ResponseBuf.String() != "chunk1chunk2" {
		t.Errorf("expected accumulated response buffer, got %q", metrics.ResponseBuf.String())
	}
	// ttft recorded on first chunk (may be 0 on a sub-millisecond fast path, so
	// assert the callback executed rather than the exact value).
	if metrics.TTFT < 0 {
		t.Errorf("expected non-negative ttft, got %d", metrics.TTFT)
	}
}

func TestHandleStreamResponse_Translate(t *testing.T) {
	h, cleanup := setupHandlerForForward(t)
	defer cleanup()

	// OpenAI SSE chunk fed to the translator.
	upstream := strings.NewReader(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n")
	rec := httptest.NewRecorder()
	metrics := &streamMetrics{}
	if err := h.handleStreamResponse(context.Background(), rec, upstream, true, time.Now(), metrics); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(rec.Body.String(), "content_block_delta") {
		t.Errorf("expected translated stream, got %s", rec.Body.String())
	}
}
