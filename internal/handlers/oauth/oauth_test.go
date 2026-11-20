package oauth

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"9router/proxy/internal/db"
)

func setupOAuthTestDB(t *testing.T) (*sql.DB, func()) {
	tmpFile, err := os.CreateTemp("", "test_oauth_*.sqlite")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	tmpFile.Close()

	database, err := db.OpenDatabase(tmpFile.Name())
	if err != nil {
		os.Remove(tmpFile.Name())
		t.Fatalf("OpenDatabase failed: %v", err)
	}

	schema := `
	CREATE TABLE IF NOT EXISTS providerConnections (
		id TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		authType TEXT NOT NULL,
		name TEXT,
		isActive INTEGER DEFAULT 1,
		data TEXT NOT NULL,
		createdAt TEXT,
		updatedAt TEXT
	);`
	if _, err := database.Exec(schema); err != nil {
		database.Close()
		os.Remove(tmpFile.Name())
		t.Fatalf("exec schema failed: %v", err)
	}

	cleanup := func() {
		database.Close()
		os.Remove(tmpFile.Name())
	}
	return database, cleanup
}

func TestHandleOAuthImport_missingProvider(t *testing.T) {
	handler := NewOAuthHandler(nil)
	req := httptest.NewRequest("POST", "/api/oauth//import", strings.NewReader(`{"accessToken":"test"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.HandleOAuthImport(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleOAuthImport_missingToken(t *testing.T) {
	handler := NewOAuthHandler(nil)
	req := httptest.NewRequest("POST", "/api/oauth/codex/import", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.HandleOAuthImport(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleOAuthImport_codex(t *testing.T) {
	database, cleanup := setupOAuthTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)
	handler := NewOAuthHandler(repo)

	body := `{"accessToken":"sk-codex-test","name":"My Codex"}`
	req := httptest.NewRequest("POST", "/api/oauth/codex/import", strings.NewReader(body))
	req.SetPathValue("provider", "codex")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.HandleOAuthImport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["provider"] != "codex" {
		t.Errorf("expected provider=codex, got %v", resp["provider"])
	}
}

func TestHandleOAuthKiroSocialAuthorize_invalidProvider(t *testing.T) {
	handler := NewOAuthHandler(nil)
	req := httptest.NewRequest("GET", "/api/oauth/kiro/social-authorize?provider=twitter", nil)
	rec := httptest.NewRecorder()
	handler.HandleOAuthKiroSocialAuthorize(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleOAuthKiroSocialAuthorize_google(t *testing.T) {
	handler := NewOAuthHandler(nil)
	req := httptest.NewRequest("GET", "/api/oauth/kiro/social-authorize?provider=google", nil)
	rec := httptest.NewRecorder()
	handler.HandleOAuthKiroSocialAuthorize(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["provider"] != "google" {
		t.Errorf("expected provider=google, got %v", resp["provider"])
	}
	if resp["authUrl"] == nil {
		t.Error("expected authUrl")
	}
	if resp["codeVerifier"] == nil {
		t.Error("expected codeVerifier")
	}
}

func TestHandleOAuthKiroSocialExchange_missingCode(t *testing.T) {
	handler := NewOAuthHandler(nil)
	req := httptest.NewRequest("POST", "/api/oauth/kiro/social-exchange", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.HandleOAuthKiroSocialExchange(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestHandleOAuthKiroSocialExchange_invalidStateRejected(t *testing.T) {
	handler := NewOAuthHandler(nil)
	req := httptest.NewRequest("POST", "/api/oauth/kiro/social-exchange", strings.NewReader(`{"code":"abc","state":"bogus"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.HandleOAuthKiroSocialExchange(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid state, got %d", rec.Code)
	}
}

func TestHandleOAuthKiroSocialExchange_validStateRedactsTokens(t *testing.T) {
	database, cleanup := setupOAuthTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)
	handler := NewOAuthHandler(repo)

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"secret-at","refresh_token":"secret-rt","id_token":"secret-id","expires_in":3600}`))
	}))
	defer tokenSrv.Close()
	handler.TokenURL = tokenSrv.URL

	authReq := httptest.NewRequest("GET", "/api/oauth/kiro/social-authorize?provider=google", nil)
	authRec := httptest.NewRecorder()
	handler.HandleOAuthKiroSocialAuthorize(authRec, authReq)
	if authRec.Code != http.StatusOK {
		t.Fatalf("authorize failed: %d", authRec.Code)
	}
	var authResp map[string]string
	json.Unmarshal(authRec.Body.Bytes(), &authResp)

	exBody, _ := json.Marshal(map[string]string{
		"code":         "auth-code-123",
		"state":        authResp["state"],
		"codeVerifier": authResp["codeVerifier"],
	})
	exReq := httptest.NewRequest("POST", "/api/oauth/kiro/social-exchange", strings.NewReader(string(exBody)))
	exReq.Header.Set("Content-Type", "application/json")
	exRec := httptest.NewRecorder()
	handler.HandleOAuthKiroSocialExchange(exRec, exReq)
	if exRec.Code != http.StatusOK {
		t.Fatalf("exchange failed: %d %s", exRec.Code, exRec.Body.String())
	}
	respText := exRec.Body.String()
	if strings.Contains(respText, "secret-at") || strings.Contains(respText, "secret-rt") || strings.Contains(respText, "secret-id") {
		t.Errorf("raw OAuth tokens leaked in HTTP response: %s", respText)
	}

	// Reusing the same state must be rejected (single-use).
	exReq2 := httptest.NewRequest("POST", "/api/oauth/kiro/social-exchange", strings.NewReader(string(exBody)))
	exRec2 := httptest.NewRecorder()
	handler.HandleOAuthKiroSocialExchange(exRec2, exReq2)
	if exRec2.Code != http.StatusBadRequest {
		t.Errorf("expected replayed state to fail with 400, got %d", exRec2.Code)
	}
}

func TestHandleOAuthCodexBulkImport(t *testing.T) {
	database, cleanup := setupOAuthTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)
	handler := NewOAuthHandler(repo)

	body := `{"tokens":[{"accessToken":"token-one"},{"accessToken":"token-two-longer"}]}`
	req := httptest.NewRequest("POST", "/api/oauth/codex/bulk-import", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.HandleOAuthCodexBulkImport(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	count, _ := resp["count"].(float64)
	if count != 2 {
		t.Errorf("expected count=2, got %v", count)
	}
}

func TestTitleProvider(t *testing.T) {
	if titleProvider("google") != "Google" {
		t.Errorf("expected Google, got %s", titleProvider("google"))
	}
	if titleProvider("github") != "GitHub" {
		t.Errorf("expected GitHub, got %s", titleProvider("github"))
	}
	if titleProvider("other") != "other" {
		t.Errorf("expected other, got %s", titleProvider("other"))
	}
}
