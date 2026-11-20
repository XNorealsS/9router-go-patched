package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"9router/proxy/internal/db"
	"9router/proxy/internal/providers"
	"9router/proxy/internal/proxy/oauth"
)

func TestGetClientForConnection_SupportsSOCKS5AndStrictProxyFailsClosed(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	if _, err := database.Exec(`CREATE TABLE IF NOT EXISTS proxyPools (
		id TEXT PRIMARY KEY,
		isActive INTEGER DEFAULT 1,
		testStatus TEXT,
		data TEXT NOT NULL,
		createdAt TEXT NOT NULL,
		updatedAt TEXT NOT NULL
	);`); err != nil {
		t.Fatalf("create proxyPools: %v", err)
	}

	repo := db.NewRepo(database)
	pool, err := repo.InsertProxyPool(db.ProxyPoolData{
		Name:        "socks-pool",
		ProxyURL:    "socks5://127.0.0.1:1080",
		Type:        "socks5",
		StrictProxy: true,
	})
	if err != nil {
		t.Fatalf("insert pool: %v", err)
	}

	h := NewChatHandler(repo)
	client, err := h.getClientForConnectionChecked(&ConnectionData{ProxyPoolID: pool["id"].(string)})
	if err != nil {
		t.Fatalf("expected socks5 proxy pool to succeed, got %v", err)
	}
	if client == h.Client {
		t.Fatal("expected dedicated proxy client, got default h.Client")
	}

	badPool, err := repo.InsertProxyPool(db.ProxyPoolData{
		Name:        "bad-strict-pool",
		ProxyURL:    "ftp://127.0.0.1:21",
		Type:        "ftp",
		StrictProxy: true,
	})
	if err != nil {
		t.Fatalf("insert bad pool: %v", err)
	}
	if _, err := h.getClientForConnectionChecked(&ConnectionData{ProxyPoolID: badPool["id"].(string)}); err == nil {
		t.Fatal("expected strictProxy with unsupported scheme to fail closed")
	}
}

func TestAntigravity_NoProjectID_ReturnsExplicitError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "loadCodeAssist") {
			w.Write([]byte(`{"allowedTiers":[{"id":"standard-tier","isDefault":true}]}`))
			return
		}
		w.Write([]byte(`{"done":true,"response":{"cloudaicompanionProject":{}}}`))
	}))
	defer srv.Close()

	oldL, oldO, oldDelay := loadCodeAssistURL, onboardUserURL, antigravityProbeDelay
	loadCodeAssistURL, onboardUserURL, antigravityProbeDelay = srv.URL+"/loadCodeAssist", srv.URL+"/onboardUser", time.Millisecond
	defer func() {
		loadCodeAssistURL, onboardUserURL, antigravityProbeDelay = oldL, oldO, oldDelay
	}()

	database, cleanup := setupChatTestDB(t)
	defer cleanup()
	seedConnDB(t, database, "antigravity", "ag-noproj", "ya29.test", srv.URL)
	projectNoCache.Delete("ag-noproj")

	h := NewChatHandler(db.NewRepo(database))
	rec := httptest.NewRecorder()
	err := h.forwardGeminiNativeRequestWithClient(
		context.Background(), h.Client, rec, "antigravity",
		&providers.ProviderConfig{BaseURL: srv.URL, Format: "gemini-native"},
		"ya29.test", "ag-noproj", []byte(`{"model":"gemini-3-flash","messages":[{"role":"user","content":"hi"}]}`),
		false, false, &streamMetrics{},
	)
	if err == nil || !strings.Contains(err.Error(), "no project ID") {
		t.Fatalf("expected explicit no project ID error, got %v", err)
	}
}

func TestAntigravity_ConcurrentRefreshSerializedAndRotatesToken(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()

	expired := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	dataJSON, _ := json.Marshal(map[string]any{
		"accessToken":  "old-at",
		"refreshToken": "old-rt",
		"expiresAt":    expired,
		"projectId":    "real-proj-1",
	})
	if _, err := database.Exec(`INSERT INTO providerConnections (id, provider, authType, name, priority, isActive, data, createdAt, updatedAt) VALUES ('conn-race', 'test-oauth-race', 'oauth', 'Race', 1, 1, ?, '2026-07-18T00:00:00Z', '2026-07-18T00:00:00Z')`, string(dataJSON)); err != nil {
		t.Fatalf("seed conn: %v", err)
	}

	var calls atomic.Int32
	oauth.Register("test-oauth-race", func(ctx context.Context, p *oauth.Params) (*oauth.TokenResult, error) {
		calls.Add(1)
		time.Sleep(25 * time.Millisecond)
		return &oauth.TokenResult{
			AccessToken:  "fresh-at",
			RefreshToken: "rotated-rt",
			ExpiresIn:    3600,
		}, nil
	})

	h := NewChatHandler(db.NewRepo(database))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, pid, err := h.refreshOAuthTokenIfExpired("conn-race", "old-at")
			if err != nil {
				t.Errorf("refresh error: %v", err)
				return
			}
			if tok != "fresh-at" {
				t.Errorf("expected fresh-at, got %q", tok)
			}
			if pid != "real-proj-1" {
				t.Errorf("expected preserved projectId real-proj-1, got %q", pid)
			}
		}()
	}
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 refresh call under concurrency, got %d", got)
	}

	var updatedRaw string
	if err := database.QueryRow(`SELECT data FROM providerConnections WHERE id = 'conn-race'`).Scan(&updatedRaw); err != nil {
		t.Fatalf("query updated data: %v", err)
	}
	var updated map[string]any
	json.Unmarshal([]byte(updatedRaw), &updated)
	if updated["refreshToken"] != "rotated-rt" {
		t.Errorf("expected rotated refreshToken persisted, got %v", updated["refreshToken"])
	}
}

func TestResetProviderHealth_UnlocksDottedModelNames(t *testing.T) {
	database, cleanup := setupChatTestDB(t)
	defer cleanup()
	seedConnDB(t, database, "antigravity", "conn-dot", "ya29.test", "https://example.com")

	repo := db.NewRepo(database)
	if err := repo.LockConnectionModel("conn-dot", "gemini-3.5-flash", 60, 1); err != nil {
		t.Fatalf("lock failed: %v", err)
	}
	locked, err := repo.IsConnectionModelLocked("conn-dot", "gemini-3.5-flash")
	if err != nil || !locked {
		t.Fatalf("expected locked=true, got %v (err=%v)", locked, err)
	}
	if err := repo.ResetProviderHealth("antigravity", "gemini-3.5-flash"); err != nil {
		t.Fatalf("reset failed: %v", err)
	}
	lockedAfter, err := repo.IsConnectionModelLocked("conn-dot", "gemini-3.5-flash")
	if err != nil || lockedAfter {
		t.Fatalf("expected locked=false after reset, got %v (err=%v)", lockedAfter, err)
	}
}

func TestGeminiStream_EmitsDoneAndUniqueToolIndices(t *testing.T) {
	sseInput := strings.Join([]string{
		`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file_ide","args":{"path":"a"}}},{"functionCall":{"name":"write_file_ide","args":{"path":"b"}}}]}}]}}`,
		"",
		`data: {"response":{"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3}}}`,
		"",
	}, "\n")

	database, cleanup := setupChatTestDB(t)
	defer cleanup()
	h := NewChatHandler(db.NewRepo(database))
	rec := httptest.NewRecorder()
	metrics := &streamMetrics{}

	if err := h.handleGeminiStream(context.Background(), rec, strings.NewReader(sseInput), false, metrics); err != nil {
		t.Fatalf("handleGeminiStream failed: %v", err)
	}
	out := rec.Body.String()
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Errorf("expected stream to terminate with data: [DONE]\\n\\n, got %q", out)
	}
	if !strings.Contains(out, `"index":0`) || !strings.Contains(out, `"index":1`) {
		t.Errorf("expected distinct tool_call indices 0 and 1, got %s", out)
	}
	if strings.Contains(out, `"read_file_ide"`) || strings.Contains(out, `"write_file_ide"`) {
		t.Errorf("expected _ide suffix stripped from tool names, got %s", out)
	}
}
