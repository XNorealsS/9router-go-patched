package chat

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"9router/proxy/internal/constants"
	"9router/proxy/internal/log"
	"9router/proxy/internal/models"
	"9router/proxy/internal/providers"
	internalproxy "9router/proxy/internal/proxy"
)

// GetBestConnection retrieves the highest-priority active connection for a provider.
// When connectionID is non-empty, it fetches that specific connection directly.
func (h *ChatHandler) GetBestConnection(provider string, connectionID string, excludeIDs []string, model string) (*models.ProviderConnection, *ConnectionData, error) {
	return h.getBestConnection(provider, connectionID, excludeIDs, model)
}

func (h *ChatHandler) getBestConnection(provider string, connectionID string, excludeIDs []string, model string) (*models.ProviderConnection, *ConnectionData, error) {
	if model != "" && !h.Repo.IsProviderAvailable(provider, model) {
		log.Warn("health", "unhealthy provider", "provider", provider, "model", model)
	}

	var conn *models.ProviderConnection
	var err error

	if connectionID != "" {
		conn, err = h.Repo.GetProviderConnectionByID(connectionID)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to fetch connection %s: %w", connectionID, err)
		}
		if conn == nil {
			return nil, nil, fmt.Errorf("connection %s not found", connectionID)
		}
	} else {
		connections, queryErr := h.Repo.GetProviderConnections(provider, true)
		if queryErr != nil {
			return nil, nil, fmt.Errorf("failed to query connections for %s: %w", provider, queryErr)
		}
		if len(connections) == 0 {
			if cfg, ok := providers.KnownProviders[provider]; ok && cfg.NoAuth {
				// Inject virtual connection for no-auth provider with optional proxy pool strategy from settings
				connData := &ConnectionData{
					AccessToken: "public",
				}
				settings, err := h.Repo.GetSettings()
				if err == nil && settings != nil && settings.ProviderStrategies != nil {
					if strat, ok := settings.ProviderStrategies[provider]; ok {
						if strat.ProxyPoolID != "" && strat.ProxyPoolID != "__none__" {
							connData.ProxyPoolID = strat.ProxyPoolID
						}
					}
				}
				publicName := "Public"
				conn := &models.ProviderConnection{
					ID:       "noauth",
					Provider: provider,
					Name:     &publicName,
					IsActive: 1,
				}
				return conn, connData, nil
			}
			return nil, nil, fmt.Errorf("no active connections for provider: %s", provider)
		}

		excludeSet := make(map[string]bool, len(excludeIDs))
		for _, id := range excludeIDs {
			excludeSet[id] = true
		}

		conn = nil
		for _, c := range connections {
			if excludeSet[c.ID] {
				continue
			}
			// Skip connections that have an active per-connection model lock
			if model != "" {
				if locked, _ := h.Repo.IsConnectionModelLocked(c.ID, model); locked {
					continue
				}
			}
			conn = c
			break
		}
		if conn == nil {
			return nil, nil, fmt.Errorf("no available connections for provider: %s (all excluded)", provider)
		}
	}

	var connData ConnectionData
	if conn.Data != "" {
		if err := json.Unmarshal([]byte(conn.Data), &connData); err != nil {
			return nil, nil, fmt.Errorf("failed to parse connection data: %w", err)
		}
	}

	return conn, &connData, nil
}

// GetProviderConfig returns the upstream configuration for a provider.
func (h *ChatHandler) GetProviderConfig(provider string, connData *ConnectionData) (*providers.ProviderConfig, error) {
	return h.getProviderConfig(provider, connData)
}

func (h *ChatHandler) getProviderConfig(provider string, connData *ConnectionData) (*providers.ProviderConfig, error) {
	var baseCfg *providers.ProviderConfig

	if connData != nil && connData.BaseURL != "" {
		if cfg, ok := providers.KnownProviders[provider]; ok {
			cloned := cfg
			cloned.BaseURL = connData.BaseURL
			baseCfg = &cloned
		} else {
			baseCfg = &providers.ProviderConfig{
				BaseURL:    connData.BaseURL,
				AuthHeader: constants.HeaderAuthorization,
				AuthScheme: constants.AuthSchemeBearer,
			}
		}
	} else if cfg, ok := providers.KnownProviders[provider]; ok {
		// Clone config so per-request headers don't mutate global registry
		cloned := cfg
		baseCfg = &cloned
	} else {
		node, nodeData, err := h.Repo.GetProviderNodeByID(provider)
		if err != nil {
			return nil, fmt.Errorf("failed to look up provider node %s: %w", provider, err)
		}
		if node != nil && nodeData != nil && nodeData.BaseURL != "" {
			baseURL := nodeData.BaseURL
			if !strings.HasSuffix(baseURL, "/chat/completions") {
				if strings.HasSuffix(baseURL, "/v1") || strings.HasSuffix(baseURL, "/v1/") {
					baseURL = strings.TrimRight(baseURL, "/") + "/chat/completions"
				} else {
					baseURL = strings.TrimRight(baseURL, "/") + "/v1/chat/completions"
				}
			}
			baseCfg = &providers.ProviderConfig{
				BaseURL:    baseURL,
				AuthHeader: constants.HeaderAuthorization,
				AuthScheme: constants.AuthSchemeBearer,
			}
		}
	}

	if baseCfg == nil {
		return nil, fmt.Errorf("provider %q has no baseUrl in connection data and is not in KnownProviders", provider)
	}

	// Check if this connection uses an Edge Relay Proxy Pool (Vercel, Cloudflare, Deno)
	if connData != nil {
		var relayURL string
		var noProxy string
		var relaySecret string

		if connData.ProxyPoolID != "" {
			if pool, err := h.Repo.GetProxyPool(connData.ProxyPoolID); err == nil && pool != nil && pool.IsActive {
				if pool.Type == "vercel" || pool.Type == "cloudflare" || pool.Type == "deno" {
					relayURL = pool.NextURL()
					noProxy = pool.NoProxy
					relaySecret = pool.RelaySecret
				}
			}
		}
		if relayURL == "" && connData.ProviderSpecificData != nil {
			if u, ok := connData.ProviderSpecificData["vercelRelayUrl"].(string); ok && u != "" {
				relayURL = u
				if np, ok := connData.ProviderSpecificData["connectionNoProxy"].(string); ok {
					noProxy = np
				}
			}
		}

		if relayURL != "" && !internalproxy.ShouldBypassNoProxy(baseCfg.BaseURL, noProxy) {
			cloned := *baseCfg
			cloned.StaticHeaders = internalproxy.BuildEdgeRelayHeaders(baseCfg.BaseURL, cloned.StaticHeaders)
			if relaySecret != "" {
				cloned.StaticHeaders["x-relay-secret"] = relaySecret
			}
			cloned.BaseURL = relayURL
			return &cloned, nil
		}
	}

	return baseCfg, nil
}

// ExtractAPIKey gets the API key from a connection's data.
func ExtractAPIKey(connData *ConnectionData) string {
	return extractAPIKey(connData)
}

func extractAPIKey(connData *ConnectionData) string {
	if connData.APIKey != "" {
		return connData.APIKey
	}
	return connData.AccessToken
}

// GetClientForConnection returns an http.Client configured with ProxyPool transport if set.
func (h *ChatHandler) GetClientForConnection(connData *ConnectionData) *http.Client {
	client, _ := h.getClientForConnectionChecked(connData)
	if client != nil {
		return client
	}
	return h.Client
}

func (h *ChatHandler) getClientForConnection(connData *ConnectionData) *http.Client {
	return h.GetClientForConnection(connData)
}

func (h *ChatHandler) getClientForConnectionChecked(connData *ConnectionData) (*http.Client, error) {
	if connData == nil {
		return h.Client, nil
	}

	var proxyURLStr string
	var proxyType string
	var strictProxy bool

	// 1. Resolve from ProxyPool
	if connData.ProxyPoolID != "" {
		pool, err := h.Repo.GetProxyPool(connData.ProxyPoolID)
		if err == nil && pool != nil && pool.IsActive {
			proxyURLStr = pool.NextURL()
			proxyType = strings.ToLower(strings.TrimSpace(pool.Type))
			strictProxy = pool.StrictProxy
		} else if err != nil {
			log.Warn("proxy", "lookup proxy pool failed", "pool", connData.ProxyPoolID, "error", err)
		}
	}

	// 2. Fallback to legacy connection proxy
	if proxyURLStr == "" {
		proxyEnabled := connData.ConnectionProxyEnabled
		proxyURL := connData.ConnectionProxyURL
		if !proxyEnabled && connData.ProviderSpecificData != nil {
			if en, ok := connData.ProviderSpecificData["connectionProxyEnabled"].(bool); ok {
				proxyEnabled = en
			}
			if u, ok := connData.ProviderSpecificData["connectionProxyUrl"].(string); ok {
				proxyURL = u
			}
			if sp, ok := connData.ProviderSpecificData["strictProxy"].(bool); ok {
				strictProxy = sp
			}
		}
		if proxyEnabled && proxyURL != "" {
			proxyURLStr = proxyURL
			proxyType = "http"
		}
	}

	if proxyURLStr == "" {
		return h.Client, nil
	}

	// Edge Relays (vercel, cloudflare, deno) rewrite URL and headers in getProviderConfig.
	if proxyType == "vercel" || proxyType == "cloudflare" || proxyType == "deno" {
		return h.Client, nil
	}

	parsedURL, err := url.Parse(proxyURLStr)
	if err != nil || parsedURL.Scheme == "" || parsedURL.Host == "" {
		log.Warn("proxy", "invalid proxy pool url", "pool", connData.ProxyPoolID, "error", err)
		if strictProxy {
			return nil, fmt.Errorf("strict proxy enabled but proxy URL is invalid")
		}
		return h.Client, nil
	}

	scheme := strings.ToLower(parsedURL.Scheme)
	switch scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		log.Warn("proxy", "unsupported proxy scheme", "pool", connData.ProxyPoolID, "scheme", scheme, "url", parsedURL.Redacted())
		if strictProxy {
			return nil, fmt.Errorf("strict proxy enabled but proxy scheme %q is unsupported", scheme)
		}
		return h.Client, nil
	}

	cacheKey := parsedURL.String()
	h.proxyClientsMu.RLock()
	if h.proxyClients != nil {
		if cached, ok := h.proxyClients[cacheKey]; ok {
			h.proxyClientsMu.RUnlock()
			return cached, nil
		}
	}
	h.proxyClientsMu.RUnlock()

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(parsedURL)
	transport.ResponseHeaderTimeout = 2 * time.Minute

	var timeout time.Duration
	if h.Client != nil {
		timeout = h.Client.Timeout
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}

	h.proxyClientsMu.Lock()
	if h.proxyClients == nil {
		h.proxyClients = make(map[string]*http.Client)
	}
	if existing, ok := h.proxyClients[cacheKey]; ok {
		h.proxyClientsMu.Unlock()
		return existing, nil
	}
	h.proxyClients[cacheKey] = client
	h.proxyClientsMu.Unlock()

	return client, nil
}
