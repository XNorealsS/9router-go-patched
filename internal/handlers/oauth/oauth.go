package oauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"9router/proxy/internal/db"
	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/log"
)

const (
	maxOAuthBodyBytes = 10 << 20 // 10 MiB
	maxTokenRespBytes = 1 << 20  // 1 MiB
	oauthStateTTL     = 10 * time.Minute
)

type pendingOAuthState struct {
	CodeVerifier string
	Provider     string
	ExpiresAt    time.Time
}

// OAuthHandler handles OAuth token import and social auth exchange endpoints.
type OAuthHandler struct {
	Repo          *db.Repo
	Client        *http.Client
	TokenURL      string // defaults to Kiro Cognito endpoint; overridable in tests
	stateMu       sync.Mutex
	pendingStates map[string]pendingOAuthState
}

// NewOAuthHandler initializes an OAuthHandler.
func NewOAuthHandler(repo *db.Repo) *OAuthHandler {
	return &OAuthHandler{
		Repo:          repo,
		Client:        &http.Client{Timeout: 15 * time.Second},
		TokenURL:      "https://kiro-auth-pool.auth.us-east-1.amazoncognito.com/oauth2/token",
		pendingStates: make(map[string]pendingOAuthState),
	}
}

// HandleOAuthImport saves credentials from CLI token import (Codex, Cursor, GitLab, etc.).
// POST /api/oauth/{provider}/import
func (h *OAuthHandler) HandleOAuthImport(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	if provider == "" {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "missing provider")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxOAuthBodyBytes))
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken,omitempty"`
		APIKey       string `json:"apiKey,omitempty"`
		MachineID    string `json:"machineId,omitempty"`
		Name         string `json:"name,omitempty"`
		ProjectID    string `json:"projectId,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	credential := req.AccessToken
	if credential == "" {
		credential = req.APIKey
	}
	if credential == "" {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "missing accessToken or apiKey")
		return
	}

	connName := req.Name
	if connName == "" {
		connName = provider + " import"
	}

	randSuffix, err := randomString(12)
	if err != nil {
		log.Error("oauth", "CSPRNG failure", "error", err)
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "failed to generate connection id")
		return
	}
	connID := provider + "-import-" + randSuffix

	// Build data JSON with provider-specific fields
	authType := "apikey"
	dataFields := map[string]any{
		"apiKey": credential,
	}
	if req.AccessToken != "" {
		dataFields["accessToken"] = req.AccessToken
	}
	if req.RefreshToken != "" {
		dataFields["refreshToken"] = req.RefreshToken
		authType = "oauth"
	}
	if req.ProjectID != "" {
		dataFields["projectId"] = req.ProjectID
	}
	if req.MachineID != "" {
		dataFields["providerSpecificData"] = map[string]any{
			"machineId": req.MachineID,
		}
	}

	data, err := json.Marshal(dataFields)
	if err != nil {
		log.Error("oauth", "marshal import data failed", "provider", provider, "error", err)
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "failed to process connection data")
		return
	}

	now := currentTimestamp()
	_, err = h.Repo.RawDB().Exec(
		`INSERT INTO providerConnections (id, provider, authType, name, isActive, data, createdAt, updatedAt) VALUES (?, ?, ?, ?, 1, ?, ?, ?)`,
		connID, provider, authType, connName, string(data), now, now,
	)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, fmt.Sprintf("save connection: %v", err))
		return
	}

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"id":         connID,
		"provider":   provider,
		"name":       connName,
		"connection": connID,
	})
}

// HandleOAuthKiroSocialAuthorize generates Kiro social auth URL with PKCE.
// GET /api/oauth/kiro/social-authorize?provider=google|github
func (h *OAuthHandler) HandleOAuthKiroSocialAuthorize(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("provider")
	if p != "google" && p != "github" {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "invalid provider, use 'google' or 'github'")
		return
	}

	// Generate PKCE challenge and single-use CSRF state
	codeVerifier, err := randomString(64)
	if err != nil {
		log.Error("oauth", "CSPRNG failure generating PKCE verifier", "error", err)
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "failed to generate PKCE verifier")
		return
	}
	codeChallenge := sha256Base64(codeVerifier)
	state, err := randomString(32)
	if err != nil {
		log.Error("oauth", "CSPRNG failure generating OAuth state", "error", err)
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "failed to generate OAuth state")
		return
	}

	h.storeState(state, p, codeVerifier)

	// Build Kiro social auth URL (AWS Cognito hosted UI)
	clientID := os.Getenv("KIRO_OAUTH_CLIENT_ID")
	if clientID == "" {
		clientID = "38k1nvcot3m5po4oi5f1jt0s46"
	}
	redirectURI := "kiro://oauth"
	params := url.Values{
		"identity_provider":     {titleProvider(p)},
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {"openid email profile"},
		"state":                 {state},
		"code_challenge_method": {"S256"},
		"code_challenge":        {codeChallenge},
	}
	authURL := "https://kiro-auth-pool.auth.us-east-1.amazoncognito.com/oauth2/authorize?" + params.Encode()

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"authUrl":       authURL,
		"state":         state,
		"codeVerifier":  codeVerifier,
		"codeChallenge": codeChallenge,
		"provider":      p,
	})
}

func (h *OAuthHandler) storeState(state, provider, codeVerifier string) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	if h.pendingStates == nil {
		h.pendingStates = make(map[string]pendingOAuthState)
	}
	now := time.Now()
	for k, v := range h.pendingStates {
		if now.After(v.ExpiresAt) {
			delete(h.pendingStates, k)
		}
	}
	h.pendingStates[state] = pendingOAuthState{
		CodeVerifier: codeVerifier,
		Provider:     provider,
		ExpiresAt:    now.Add(oauthStateTTL),
	}
}

func (h *OAuthHandler) consumeState(state string) (pendingOAuthState, bool) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	if h.pendingStates == nil || state == "" {
		return pendingOAuthState{}, false
	}
	entry, ok := h.pendingStates[state]
	if !ok {
		return pendingOAuthState{}, false
	}
	delete(h.pendingStates, state)
	if time.Now().After(entry.ExpiresAt) {
		return pendingOAuthState{}, false
	}
	return entry, true
}

// HandleOAuthKiroSocialExchange exchanges auth code for Kiro tokens.
// POST /api/oauth/kiro/social-exchange
func (h *OAuthHandler) HandleOAuthKiroSocialExchange(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxOAuthBodyBytes))
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req struct {
		Code         string `json:"code"`
		State        string `json:"state"`
		CodeVerifier string `json:"codeVerifier"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.Code == "" {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "missing code")
		return
	}
	pending, ok := h.consumeState(req.State)
	if !ok {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "invalid or expired OAuth state")
		return
	}
	verifier := req.CodeVerifier
	if verifier == "" {
		verifier = pending.CodeVerifier
	} else if pending.CodeVerifier != "" && verifier != pending.CodeVerifier {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "PKCE code_verifier mismatch")
		return
	}

	clientID := os.Getenv("KIRO_OAUTH_CLIENT_ID")
	if clientID == "" {
		clientID = "38k1nvcot3m5po4oi5f1jt0s46"
	}

	tokenURL := h.TokenURL
	if tokenURL == "" {
		tokenURL = "https://kiro-auth-pool.auth.us-east-1.amazoncognito.com/oauth2/token"
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {clientID},
		"code":          {req.Code},
		"redirect_uri":  {"kiro://oauth"},
		"code_verifier": {verifier},
	}

	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "failed to create token exchange request")
		return
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")

	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	tokenResp, err := client.Do(httpReq)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadGateway, fmt.Sprintf("token exchange failed: %v", err))
		return
	}
	defer tokenResp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(tokenResp.Body, maxTokenRespBytes))
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadGateway, "failed to read token response")
		return
	}
	if tokenResp.StatusCode != http.StatusOK {
		handlerutil.WriteJSONError(w, http.StatusBadGateway, fmt.Sprintf("token exchange returned %d", tokenResp.StatusCode))
		return
	}

	var tokenData map[string]any
	if err := json.Unmarshal(respBytes, &tokenData); err != nil {
		log.Error("oauth", "decode token response failed", "error", err)
		handlerutil.WriteJSONError(w, http.StatusBadGateway, "failed to decode token response")
		return
	}
	accessToken, ok := tokenData["access_token"].(string)
	if !ok || accessToken == "" {
		handlerutil.WriteJSONError(w, http.StatusBadGateway, "token response missing access_token")
		return
	}

	randSuffix, err := randomString(12)
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "failed to generate connection id")
		return
	}
	connID := "kiro-oauth-" + randSuffix
	dataMap := map[string]any{
		"accessToken": accessToken,
	}
	if idToken, ok := tokenData["id_token"].(string); ok && idToken != "" {
		dataMap["idToken"] = idToken
	}
	if refreshToken, ok := tokenData["refresh_token"].(string); ok && refreshToken != "" {
		dataMap["refreshToken"] = refreshToken
	}
	if expiresIn, ok := tokenData["expires_in"].(float64); ok && expiresIn > 0 {
		dataMap["expiresAt"] = time.Now().Add(time.Duration(expiresIn) * time.Second).Format(time.RFC3339)
	}
	data, err := json.Marshal(dataMap)
	if err != nil {
		log.Error("oauth", "marshal Kiro social data failed", "error", err)
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, "failed to encode connection data")
		return
	}
	if h.Repo != nil {
		now := currentTimestamp()
		if _, err := h.Repo.RawDB().Exec(
			`INSERT INTO providerConnections (id, provider, authType, name, isActive, data, createdAt, updatedAt) VALUES (?, ?, 'oauth', ?, 1, ?, ?, ?)`,
			connID, "kiro", "Kiro Social", string(data), now, now,
		); err != nil {
			log.Error("oauth", "save Kiro social connection failed", "error", err)
			handlerutil.WriteJSONError(w, http.StatusInternalServerError, "failed to save Kiro connection")
			return
		}
	}

	// Never echo raw OAuth tokens back to the caller in HTTP responses.
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"id":       connID,
		"provider": "kiro",
		"status":   "connected",
	})
}

// HandleOAuthCodexBulkImport handles bulk Codex token import.
// POST /api/oauth/codex/bulk-import
func (h *OAuthHandler) HandleOAuthCodexBulkImport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxOAuthBodyBytes))
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	defer r.Body.Close()

	var req struct {
		Tokens []struct {
			AccessToken string `json:"accessToken"`
			Name        string `json:"name,omitempty"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	var imported []string
	for _, t := range req.Tokens {
		if t.AccessToken == "" {
			continue
		}
		name := t.Name
		if name == "" {
			name = "Codex import"
		}
		randSuffix, err := randomString(12)
		if err != nil {
			log.Error("oauth", "CSPRNG failure during bulk import", "error", err)
			continue
		}
		connID := "codex-bulk-" + randSuffix
		data, err := json.Marshal(map[string]string{"accessToken": t.AccessToken})
		if err != nil {
			log.Error("oauth", "marshal Codex bulk import failed", "error", err)
			continue
		}
		now := currentTimestamp()
		_, err = h.Repo.RawDB().Exec(
			`INSERT INTO providerConnections (id, provider, authType, name, isActive, data, createdAt, updatedAt) VALUES (?, 'codex', 'oauth', ?, 1, ?, ?, ?)`,
			connID, name, string(data), now, now,
		)
		if err == nil {
			imported = append(imported, connID)
		}
	}

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"imported": imported,
		"count":    len(imported),
	})
}

func titleProvider(p string) string {
	if p == "google" {
		return "Google"
	}
	if p == "github" {
		return "GitHub"
	}
	return p
}

func currentTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func randomString(n int) (string, error) {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = letters[int(b[i])%len(letters)]
	}
	return string(b), nil
}

func sha256Base64(input string) string {
	h := sha256.Sum256([]byte(input))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
