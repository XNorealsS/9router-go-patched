package middleware

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"9router/proxy/internal/db"
	"9router/proxy/internal/dbtest"
)

func TestRequireGatewayAuth_DashboardSessionAndAPIKey(t *testing.T) {
	database, err := db.OpenDatabase(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	for _, stmt := range dbtest.SchemaStatements() {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	_, err = database.Exec(`INSERT INTO apiKeys (id, key, name, isActive, createdAt) VALUES ('k1', 'sk-gw-1', 'admin', 1, '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatalf("seed key: %v", err)
	}
	repo := db.NewRepo(database)

	tmpDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmpDir, "jwt-secret"), []byte("super-test-jwt-secret-hex"), 0600); err != nil {
		t.Fatalf("write jwt-secret: %v", err)
	}

	okHandler := RequireGatewayAuth(repo, tmpDir)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// 1. Unauthenticated -> 401
	rec := httptest.NewRecorder()
	okHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/gateway/system", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without session or key, got %d", rec.Code)
	}

	// 2. Valid minted Next.js-compatible HS256 auth_token cookie -> 200
	toks := MintLoopbackAuthTokens(tmpDir)
	if len(toks) == 0 || toks[0] == "" {
		t.Fatal("MintLoopbackAuthTokens failed")
	}
	tok := toks[0]
	req := httptest.NewRequest(http.MethodGet, "/api/gateway/system", nil)
	req.AddCookie(&http.Cookie{Name: "auth_token", Value: tok})
	rec = httptest.NewRecorder()
	okHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid dashboard cookie, got %d", rec.Code)
	}

	// 3. Tampered cookie -> 401
	reqBad := httptest.NewRequest(http.MethodGet, "/api/gateway/system", nil)
	reqBad.AddCookie(&http.Cookie{Name: "auth_token", Value: tok + "tamper"})
	rec = httptest.NewRecorder()
	okHandler.ServeHTTP(rec, reqBad)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with tampered cookie, got %d", rec.Code)
	}

	// 4. Valid API key still accepted -> 200
	reqKey := httptest.NewRequest(http.MethodGet, "/api/gateway/system", nil)
	reqKey.Header.Set("X-API-Key", "sk-gw-1")
	rec = httptest.NewRecorder()
	okHandler.ServeHTTP(rec, reqKey)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with valid API key, got %d", rec.Code)
	}
}
