package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"9router/proxy/internal/handlerutil"
)

// OAuthClientConfig holds OAuth app credentials for token refresh.
type OAuthClientConfig struct {
	ClientID     string
	ClientSecret string
	TokenURL     string
}

// OAuthConnectionData holds the persisted OAuth fields inside ConnectionData.
type OAuthConnectionData struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    string `json:"expiresAt"` // RFC3339 / ISO 8601
	ProjectID    string `json:"projectId"`
}

// OAuthTokenResponse is the JSON response from a token refresh endpoint.
type OAuthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope,omitempty"`
	TokenType    string `json:"token_type,omitempty"`
}

// KnownOAuthConfigs maps provider IDs to their OAuth client configuration for token refresh.
// Client credentials must be supplied explicitly via environment variables.
var KnownOAuthConfigs = map[string]OAuthClientConfig{
	"antigravity": {
		ClientID:     envOr("ANTIGRAVITY_OAUTH_CLIENT_ID", ""),
		ClientSecret: envOr("ANTIGRAVITY_OAUTH_CLIENT_SECRET", ""),
		TokenURL:     "https://oauth2.googleapis.com/token",
	},
	"xai": {
		ClientID:     envOr("XAI_OAUTH_CLIENT_ID", ""),
		ClientSecret: envOr("XAI_OAUTH_CLIENT_SECRET", ""),
		TokenURL:     "https://auth.x.ai/oauth2/token",
	},
	"codex": {
		ClientID:     envOr("CODEX_OAUTH_CLIENT_ID", ""),
		ClientSecret: envOr("CODEX_OAUTH_CLIENT_SECRET", ""),
		TokenURL:     "https://auth.openai.com/oauth/token",
	},
	"github": {
		ClientID:     envOr("GITHUB_OAUTH_CLIENT_ID", ""),
		ClientSecret: envOr("GITHUB_OAUTH_CLIENT_SECRET", ""),
		TokenURL:     "https://github.com/login/oauth/access_token",
	},
	"iflow": {
		ClientID:     envOr("IFLOW_OAUTH_CLIENT_ID", ""),
		ClientSecret: envOr("IFLOW_OAUTH_CLIENT_SECRET", ""),
		TokenURL:     "https://iflow.cn/oauth/token",
	},
	"gemini-cli": {
		ClientID:     envOr("GEMINI_CLI_OAUTH_CLIENT_ID", ""),
		ClientSecret: envOr("GEMINI_CLI_OAUTH_CLIENT_SECRET", ""),
		TokenURL:     "https://oauth2.googleapis.com/token",
	},
	"kimi-coding": {
		ClientID:     envOr("KIMI_CODING_OAUTH_CLIENT_ID", ""),
		ClientSecret: envOr("KIMI_CODING_OAUTH_CLIENT_SECRET", ""),
		TokenURL:     "https://auth.kimi.com/api/oauth/token",
	},
	"qoder": {
		ClientID:     envOr("QODER_OAUTH_CLIENT_ID", ""),
		ClientSecret: envOr("QODER_OAUTH_CLIENT_SECRET", ""),
		TokenURL:     "https://center.qoder.sh/algo/api/v3/user/refresh_token",
	},
	"codebuddy-cn": {
		ClientID:     envOr("CODEBUDDY_CN_OAUTH_CLIENT_ID", ""),
		ClientSecret: envOr("CODEBUDDY_CN_OAUTH_CLIENT_SECRET", ""),
		TokenURL:     "https://copilot.tencent.com/v2/plugin/auth/token/refresh",
	},
	"codebuddy-intl": {
		ClientID:     envOr("CODEBUDDY_INTL_OAUTH_CLIENT_ID", ""),
		ClientSecret: envOr("CODEBUDDY_INTL_OAUTH_CLIENT_SECRET", ""),
		TokenURL:     "https://copilot.tencent.com/v2/plugin/auth/token/refresh",
	},
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// RefreshToken performs an OAuth token refresh for the given provider configuration.
// Returns the new access token (and optional rotated refresh token) and expires_in duration.
func RefreshToken(cfg OAuthClientConfig, refreshToken string) (*OAuthTokenResponse, error) {
	return RefreshTokenWithContext(context.Background(), &http.Client{Timeout: 15 * time.Second}, cfg, refreshToken)
}

// RefreshTokenWithContext performs an OAuth token refresh using the given context and HTTP client.
func RefreshTokenWithContext(ctx context.Context, client *http.Client, cfg OAuthClientConfig, refreshToken string) (*OAuthTokenResponse, error) {
	if cfg.ClientID == "" || cfg.TokenURL == "" {
		return nil, fmt.Errorf("OAuth client credentials not configured (set provider OAUTH_CLIENT_ID/SECRET env vars)")
	}
	if refreshToken == "" {
		return nil, fmt.Errorf("missing refresh token")
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}

	values := url.Values{
		"client_id":     {cfg.ClientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	if cfg.ClientSecret != "" {
		values.Set("client_secret", cfg.ClientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, bytes.NewBufferString(values.Encode()))
	if err != nil {
		return nil, fmt.Errorf("create OAuth refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OAuth refresh POST: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read OAuth response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := string(body)
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("OAuth token refresh returned %d: %s", resp.StatusCode, snippet)
	}

	return ParseRefreshResponse(body)
}

// ParseOAuthConnection extracts OAuth fields from a ConnectionData blob.
func ParseOAuthConnection(data map[string]interface{}) *OAuthConnectionData {
	if data == nil {
		return nil
	}
	return &OAuthConnectionData{
		AccessToken:  handlerutil.GetString(data, "accessToken"),
		RefreshToken: handlerutil.GetString(data, "refreshToken"),
		ExpiresAt:    handlerutil.GetString(data, "expiresAt"),
		ProjectID:    handlerutil.GetString(data, "projectId"),
	}
}

// IsExpired checks if the OAuth token is expired or will expire within 60 seconds.
func (o *OAuthConnectionData) IsExpired() bool {
	if o.ExpiresAt == "" {
		return true
	}
	exp, err := time.Parse(time.RFC3339, o.ExpiresAt)
	if err != nil {
		return true
	}
	return time.Now().After(exp.Add(-60 * time.Second))
}

// BuildRefreshPayload builds the URL-encoded form body for a token refresh request.
func (c *OAuthClientConfig) BuildRefreshPayload(refreshToken string) string {
	return url.Values{
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}.Encode()
}

// ParseRefreshResponse parses the JSON response from a token refresh endpoint.
func ParseRefreshResponse(body []byte) (*OAuthTokenResponse, error) {
	var resp OAuthTokenResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse OAuth response: %w", err)
	}
	if resp.AccessToken == "" {
		return nil, fmt.Errorf("empty access token in OAuth response")
	}
	return &resp, nil
}

// BuildConnectionUpdate builds a partial ConnectionData map for DB update.
func (r *OAuthTokenResponse) BuildConnectionUpdate() map[string]interface{} {
	update := map[string]interface{}{
		"accessToken": r.AccessToken,
		"expiresAt":   time.Now().Add(time.Duration(r.ExpiresIn) * time.Second).Format(time.RFC3339),
	}
	if r.RefreshToken != "" {
		update["refreshToken"] = r.RefreshToken
	}
	return update
}
