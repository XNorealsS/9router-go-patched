package proxy

import (
	"context"
	"fmt"
	"net/http"

	"9router/proxy/internal/providers"
)

// ForwardOpenAI sends an OpenAI-format request to the provider endpoint.
func ForwardOpenAI(ctx context.Context, client *http.Client, cfg *providers.ProviderConfig, apiKey string, body []byte, isStream bool) (*http.Response, error) {
	headers := map[string]string{}
	if !cfg.NoAuth {
		switch cfg.AuthScheme {
		case "bearer":
			headers[cfg.AuthHeader] = "Bearer " + apiKey
		case "raw":
			headers[cfg.AuthHeader] = apiKey
		default:
			headers["Authorization"] = "Bearer " + apiKey
		}
	}
	for k, v := range cfg.StaticHeaders {
		headers[k] = v
	}
	if isStream {
		headers["Accept"] = "text/event-stream"
	}
	resp, err := DoRequest(ctx, client, "POST", cfg.BaseURL, headers, body)
	if err != nil {
		return nil, fmt.Errorf("forward to %s: %w", cfg.BaseURL, err)
	}
	return resp, nil
}
