//go:build live_antigravity

package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"9router/proxy/internal/db"
	"9router/proxy/internal/providers"
)

// Live Antigravity integration suite (Phase 9).
// Requires explicit operator-supplied credentials via environment variables:
//   - ANTIGRAVITY_OAUTH_CLIENT_ID
//   - ANTIGRAVITY_OAUTH_CLIENT_SECRET
//   - ANTIGRAVITY_LIVE_REFRESH_TOKEN (and/or ANTIGRAVITY_LIVE_ACCESS_TOKEN)
//   - ANTIGRAVITY_LIVE_PROJECT_ID (optional; discovered via loadCodeAssist if empty)
func requireLiveAntigravityCreds(t *testing.T) (accessTok, refreshTok, projectID string) {
	t.Helper()
	accessTok = os.Getenv("ANTIGRAVITY_LIVE_ACCESS_TOKEN")
	refreshTok = os.Getenv("ANTIGRAVITY_LIVE_REFRESH_TOKEN")
	projectID = os.Getenv("ANTIGRAVITY_LIVE_PROJECT_ID")
	if accessTok == "" && refreshTok == "" {
		t.Skip("BLOCKED: set ANTIGRAVITY_LIVE_ACCESS_TOKEN or ANTIGRAVITY_LIVE_REFRESH_TOKEN to run live Antigravity tests")
	}
	return accessTok, refreshTok, projectID
}

func setupLiveAntigravityHandler(t *testing.T, accessTok, refreshTok, projectID string) (*ChatHandler, func()) {
	t.Helper()
	database, cleanup := setupChatTestDB(t)
	exp := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	if accessTok == "" {
		exp = time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	}
	data, _ := json.Marshal(map[string]any{
		"accessToken":  accessTok,
		"refreshToken": refreshTok,
		"expiresAt":    exp,
		"projectId":    projectID,
	})
	if _, err := database.Exec(
		`INSERT INTO providerConnections (id, provider, authType, name, priority, isActive, data, createdAt, updatedAt) VALUES ('live-ag', 'antigravity', 'oauth', 'Live AG', 1, 1, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		string(data),
	); err != nil {
		cleanup()
		t.Fatalf("seed live connection: %v", err)
	}
	return NewChatHandler(db.NewRepo(database)), cleanup
}

// TEST A — OAuth Refresh
func TestLiveAntigravity_A_OAuthRefresh(t *testing.T) {
	_, refreshTok, _ := requireLiveAntigravityCreds(t)
	if refreshTok == "" {
		t.Skip("BLOCKED: ANTIGRAVITY_LIVE_REFRESH_TOKEN not set")
	}
	cfg := providers.KnownOAuthConfigs["antigravity"]
	if cfg.ClientID == "" {
		t.Skip("BLOCKED: ANTIGRAVITY_OAUTH_CLIENT_ID not configured")
	}
	resp, err := providers.RefreshTokenWithContext(context.Background(), nil, cfg, refreshTok)
	if err != nil {
		t.Fatalf("live OAuth refresh failed: %v", err)
	}
	if resp.AccessToken == "" {
		t.Fatal("expected non-empty access token")
	}
}

// TEST B — Basic Generation
func TestLiveAntigravity_B_BasicGeneration(t *testing.T) {
	at, rt, pid := requireLiveAntigravityCreds(t)
	h, cleanup := setupLiveAntigravityHandler(t, at, rt, pid)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{
		"model": "antigravity/gemini-3-flash",
		"messages": [{"role":"user","content":"Reply with exactly OK"}],
		"stream": false
	}`)))
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TEST C — Streaming
func TestLiveAntigravity_C_Streaming(t *testing.T) {
	at, rt, pid := requireLiveAntigravityCreds(t)
	h, cleanup := setupLiveAntigravityHandler(t, at, rt, pid)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{
		"model": "antigravity/gemini-3-flash",
		"messages": [{"role":"user","content":"Count 1 2 3"}],
		"stream": true
	}`)))
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Errorf("expected stream termination [DONE], got: %s", rec.Body.String())
	}
}

// TEST D — Tool Calling
func TestLiveAntigravity_D_ToolCalling(t *testing.T) {
	at, rt, pid := requireLiveAntigravityCreds(t)
	h, cleanup := setupLiveAntigravityHandler(t, at, rt, pid)
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{
		"model": "antigravity/gemini-3-flash",
		"messages": [{"role":"user","content":"What is the weather in Tokyo? Call get_weather."}],
		"tools": [{"type":"function","function":{"name":"get_weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],
		"stream": false
	}`)))
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TEST E — Multimodal
func TestLiveAntigravity_E_Multimodal(t *testing.T) {
	at, rt, pid := requireLiveAntigravityCreds(t)
	h, cleanup := setupLiveAntigravityHandler(t, at, rt, pid)
	defer cleanup()

	// 1x1 transparent PNG
	pngB64 := "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII="
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{
		"model": "antigravity/gemini-3-flash",
		"messages": [{"role":"user","content":[{"type":"text","text":"Describe image"},{"type":"image_url","image_url":{"url":"data:image/png;base64,`+pngB64+`"}}]}],
		"stream": false
	}`)))
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TEST F — Model Compatibility
func TestLiveAntigravity_F_ModelCompatibility(t *testing.T) {
	at, rt, pid := requireLiveAntigravityCreds(t)
	h, cleanup := setupLiveAntigravityHandler(t, at, rt, pid)
	defer cleanup()

	for _, alias := range []string{"gemini-default", "gemini-3.5-flash", "gemini-3-pro-high"} {
		t.Run(alias, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{
				"model": "antigravity/`+alias+`",
				"messages": [{"role":"user","content":"ping"}],
				"stream": false
			}`)))
			rec := httptest.NewRecorder()
			h.HandleChatCompletions(rec, req)
			t.Logf("alias=%s status=%d body=%s", alias, rec.Code, rec.Body.String())
		})
	}
}

// TEST G — Error Handling
func TestLiveAntigravity_G_InvalidAuth(t *testing.T) {
	requireLiveAntigravityCreds(t)
	h, cleanup := setupLiveAntigravityHandler(t, "ya29.invalid-token", "", "nonexistent-proj")
	defer cleanup()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{
		"model": "antigravity/gemini-3-flash",
		"messages": [{"role":"user","content":"ping"}],
		"stream": false
	}`)))
	rec := httptest.NewRecorder()
	h.HandleChatCompletions(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("expected non-200 error for invalid auth token")
	}
}

// TEST H — Long-Running Stability & Concurrency
func TestLiveAntigravity_H_ConcurrentAndCancel(t *testing.T) {
	at, rt, pid := requireLiveAntigravityCreds(t)
	h, cleanup := setupLiveAntigravityHandler(t, at, rt, pid)
	defer cleanup()

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{
				"model": "antigravity/gemini-3-flash",
				"messages": [{"role":"user","content":"Say hi"}],
				"stream": false
			}`))).WithContext(ctx)
			rec := httptest.NewRecorder()
			h.HandleChatCompletions(rec, req)
		}()
	}
	wg.Wait()
}
