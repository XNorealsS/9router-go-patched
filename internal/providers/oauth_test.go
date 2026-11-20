package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestKnownOAuthConfigs_NoHardcodedGoogleSecrets(t *testing.T) {
	for _, id := range []string{"antigravity", "gemini-cli"} {
		cfg := KnownOAuthConfigs[id]
		if cfg.ClientSecret != "" && (len(cfg.ClientSecret) >= 6 && cfg.ClientSecret[:6] == "GOCSPX") {
			t.Errorf("%s should not embed a hardcoded Google OAuth client secret", id)
		}
	}
	if cfg := KnownOAuthConfigs["iflow"]; cfg.ClientSecret != "" {
		t.Errorf("iflow should not embed a hardcoded client secret")
	}
}

func TestRefreshTokenWithContext_UnconfiguredFails(t *testing.T) {
	_, err := RefreshTokenWithContext(context.Background(), nil, OAuthClientConfig{TokenURL: "https://oauth2.googleapis.com/token"}, "rt")
	if err == nil {
		t.Fatal("expected error when ClientID is empty")
	}
}

func TestRefreshTokenWithContext_PreservesRotatedRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"access_token":"new-at","refresh_token":"rotated-rt","expires_in":3600}`))
	}))
	defer srv.Close()

	resp, err := RefreshTokenWithContext(context.Background(), srv.Client(), OAuthClientConfig{
		ClientID: "cid",
		TokenURL: srv.URL,
	}, "old-rt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	update := resp.BuildConnectionUpdate()
	if update["accessToken"] != "new-at" {
		t.Errorf("expected new-at, got %v", update["accessToken"])
	}
	if update["refreshToken"] != "rotated-rt" {
		t.Errorf("expected rotated-rt preserved in update, got %v", update["refreshToken"])
	}
}
