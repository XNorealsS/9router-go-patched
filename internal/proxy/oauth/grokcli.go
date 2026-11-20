package oauth

import (
	"context"
	"fmt"
	"net/url"
	"os"
)

func init() {
	Register("grok-cli", refreshGrokCLI)
}

// refreshGrokCLI refreshes a Grok CLI OAuth token.
func refreshGrokCLI(ctx context.Context, p *Params) (*TokenResult, error) {
	clientID := os.Getenv("XAI_OAUTH_CLIENT_ID")
	if clientID == "" {
		return nil, fmt.Errorf("grok-cli OAuth client ID not configured (set XAI_OAUTH_CLIENT_ID)")
	}
	vals := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {p.RefreshToken},
	}
	return doFormRefresh(ctx, p.Client, "https://auth.x.ai/oauth2/token", vals)
}
