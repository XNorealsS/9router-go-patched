package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"9router/proxy/internal/db"
	"9router/proxy/internal/dbtest"
)

func setupGatewayTestRouter(t *testing.T) (*chi.Mux, *db.Repo, *TokenSaverConfig) {
	t.Helper()
	database, cleanup := setupTestDB(t)
	t.Cleanup(cleanup)

	for _, stmt := range dbtest.SchemaStatements() {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("apply schema: %v", err)
		}
	}
	_, err := database.Exec(
		`INSERT INTO apiKeys (id, key, name, isActive, createdAt) VALUES ('k1', 'sk-test-gw', 'admin', 1, '2026-01-01T00:00:00Z')`,
	)
	if err != nil {
		t.Fatalf("seed apiKey: %v", err)
	}
	_, err = database.Exec(
		`INSERT INTO providerConnections (id, provider, authType, name, isActive, data, createdAt, updatedAt) VALUES ('c1', 'antigravity', 'oauth', 'ag-main', 1, '{"accessToken":"sk-secret-must-not-leak","projectId":"proj-live","modelLock_gemini-3-flash":"2099-01-01T00:00:00Z","backoffLevel":3}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	)
	if err != nil {
		t.Fatalf("seed providerConnection: %v", err)
	}

	repo := db.NewRepo(database)
	ts := NewTokenSaverConfig(true, false, false)
	r := chi.NewRouter()
	SetupServerRouterWithGateway(r, repo, ts, nil, GatewayRuntimeInfo{
		ListenAddr:   "127.0.0.1:20128",
		DashboardURL: "http://127.0.0.1:3000",
	})
	return r, repo, ts
}

func TestGatewayPanel_AuthRedactionAndControls(t *testing.T) {
	r, repo, ts := setupGatewayTestRouter(t)

	// 1. HTML shell /gateway renders Crimson/Black/White console without orange accent tokens.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/gateway", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "9ROUTER // 1-GATEWAY CONSOLE") {
		t.Fatalf("expected 200 gateway HTML, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "#dc2626") {
		t.Errorf("expected crimson red accent #dc2626 in /gateway UI")
	}

	// 2. /api/gateway/system rejects unauthenticated callers.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/gateway/system", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without API key, got %d", rec.Code)
	}

	// 3. Authenticated /api/gateway/system returns status & locks while redacting secrets.
	req := httptest.NewRequest(http.MethodGet, "/api/gateway/system", nil)
	req.Header.Set("X-API-Key", "sk-test-gw")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid API key, got %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "sk-secret-must-not-leak") {
		t.Fatalf("secret leaked in /api/gateway/system response: %s", body)
	}
	if !strings.Contains(body, "gemini-3-flash") || !strings.Contains(body, "proj-live") {
		t.Fatalf("expected active model lock and projectId in response: %s", body)
	}

	// 4. POST /api/gateway/toggles updates runtime TokenSaverConfig.
	togReq := httptest.NewRequest(http.MethodPost, "/api/gateway/toggles", bytes.NewReader([]byte(`{"caveman":true,"cavemanLevel":"ultra","injectionGuard":false}`)))
	togReq.Header.Set("X-API-Key", "sk-test-gw")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, togReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("toggles failed: %d %s", rec.Code, rec.Body.String())
	}
	if !ts.CavemanEnabled() || ts.CavemanLevel() != "ultra" || ts.InjectionGuardEnabled() {
		t.Fatalf("expected runtime toggles updated, got caveman=%v level=%s guard=%v", ts.CavemanEnabled(), ts.CavemanLevel(), ts.InjectionGuardEnabled())
	}

	// 5. POST /api/gateway/health/reset clears active model locks.
	rstReq := httptest.NewRequest(http.MethodPost, "/api/gateway/health/reset", bytes.NewReader([]byte(`{"provider":"antigravity"}`)))
	rstReq.Header.Set("X-API-Key", "sk-test-gw")
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, rstReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("health reset failed: %d %s", rec.Code, rec.Body.String())
	}
	locked, err := repo.IsConnectionModelLocked("c1", "gemini-3-flash")
	if err != nil || locked {
		t.Fatalf("expected model lock cleared after reset, locked=%v err=%v", locked, err)
	}
	var res map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &res)
	if res["status"] != "ok" {
		t.Fatalf("unexpected reset body: %v", res)
	}
}
