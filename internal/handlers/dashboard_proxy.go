package handlers

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/log"
)

// NewDashboardProxy creates a reverse proxy handler targeting a companion
// 9Router Next.js dashboard server (e.g. http://127.0.0.1:3000) so both the
// management UI and Go LLM proxy share a single HTTP gateway port.
func NewDashboardProxy(targetURL string) (http.Handler, error) {
	trimmed := strings.TrimSpace(targetURL)
	if trimmed == "" {
		return nil, nil
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid DASHBOARD_URL %q: %w", trimmed, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("invalid DASHBOARD_URL %q: must be http(s)://host[:port]", trimmed)
	}

	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.FlushInterval = -1
	origDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		origHost := req.Host
		origDirector(req)
		if origHost != "" {
			req.Header.Set("X-Forwarded-Host", origHost)
		}
		if req.TLS != nil {
			req.Header.Set("X-Forwarded-Proto", "https")
		} else if req.Header.Get("X-Forwarded-Proto") == "" {
			req.Header.Set("X-Forwarded-Proto", "http")
		}
		// Keep original browser Host header so Next.js cookies/redirects stay on gateway origin.
		if origHost != "" {
			req.Host = origHost
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, proxyErr error) {
		log.Warn("dashboard-proxy", "upstream unavailable", "target", u.String(), "path", r.URL.Path, "error", proxyErr)
		handlerutil.WriteJSONError(w, http.StatusServiceUnavailable, "Dashboard backend is unavailable. Start the companion 9router dashboard or check DASHBOARD_URL.")
	}
	return proxy, nil
}
