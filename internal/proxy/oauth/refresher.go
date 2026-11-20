// Package oauth provides per-provider OAuth token refresh.
// Register custom refresh functions for providers that need non-standard OAuth flows.
package oauth

import (
	"context"
	"net/http"
	"sync"
)

// TokenResult holds the result of a token refresh.
type TokenResult struct {
	AccessToken  string
	RefreshToken string // populated when the upstream rotates the refresh token
	ExpiresIn    int    // seconds
	Scope        string
	ProjectID    string // provider-specific extra field
}

// Params holds all inputs for a refresh call.
type Params struct {
	Client       *http.Client
	Provider     string
	RefreshToken string
	AccessToken  string // current (possibly expired) token
}

// Refresher refreshes an OAuth token for a specific provider.
type Refresher func(ctx context.Context, p *Params) (*TokenResult, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Refresher{}
)

// Register adds a refresher for the given provider.
func Register(provider string, fn Refresher) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[provider] = fn
}

// Get returns the refresher for the given provider, or nil.
func Get(provider string) Refresher {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return registry[provider]
}
