package handlers

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/log"
)

const gatewayThemeStyleTag = `<style id="gw-red-theme">
:root{--color-brand-50:#fef2f2!important;--color-brand-100:#fee2e2!important;--color-brand-200:#fecaca!important;--color-brand-300:#fca5a5!important;--color-brand-400:#f87171!important;--color-brand-500:#dc2626!important;--color-brand-600:#b91c1c!important;--color-brand-700:#991b1b!important;--color-brand-800:#7f1d1d!important;--color-brand-900:#450a0a!important;--color-orange-200:#fecaca!important;--color-orange-400:#f87171!important;--color-orange-500:#dc2626!important;--color-orange-700:#991b1b!important;--color-orange-900:#450a0a!important;--color-primary:#dc2626!important;--color-primary-hover:#b91c1c!important;--color-accent:#dc2626!important;--color-bg:#fafafa!important;--color-bg-alt:#f4f4f5!important;--color-surface:#ffffff!important;--color-surface-2:#f4f4f5!important;--color-surface-3:#e4e4e7!important;--color-sidebar:#ffffff!important;--color-border:#e4e4e7!important;--color-border-subtle:#f4f4f5!important;--color-text:#09090b!important;--color-text-main:#09090b!important;--color-text-muted:#52525b!important;--color-text-subtle:#71717a!important;--radius-brand:6px!important;--radius-brand-lg:8px!important;--shadow-warm:0 2px 10px -2px rgba(220,38,38,.18)!important;--shadow-focus:0 0 0 2px rgba(220,38,38,.35)!important}
.dark{--color-brand-50:#fef2f2!important;--color-brand-100:#fee2e2!important;--color-brand-200:#fecaca!important;--color-brand-300:#fca5a5!important;--color-brand-400:#f87171!important;--color-brand-500:#dc2626!important;--color-brand-600:#b91c1c!important;--color-brand-700:#991b1b!important;--color-brand-800:#7f1d1d!important;--color-brand-900:#450a0a!important;--color-orange-200:#fecaca!important;--color-orange-400:#f87171!important;--color-orange-500:#dc2626!important;--color-orange-700:#991b1b!important;--color-orange-900:#450a0a!important;--color-primary:#dc2626!important;--color-primary-hover:#b91c1c!important;--color-accent:#dc2626!important;--color-bg:#09090b!important;--color-bg-alt:#0f0f12!important;--color-surface:#121216!important;--color-surface-2:#18181d!important;--color-surface-3:#222228!important;--color-sidebar:#09090b!important;--color-border:#27272a!important;--color-border-subtle:#18181b!important;--color-text:#fafafa!important;--color-text-main:#fafafa!important;--color-text-muted:#a1a1aa!important;--color-text-subtle:#71717a!important;--shadow-warm:0 2px 10px -2px rgba(220,38,38,.25)!important;--shadow-focus:0 0 0 2px rgba(220,38,38,.4)!important}
#gw-console-link{position:fixed;right:16px;bottom:16px;z-index:9999;display:inline-flex;align-items:center;gap:8px;padding:8px 14px;background:#09090b;color:#fafafa;border:1px solid #dc2626;border-radius:4px;font:600 12px/1.2 ui-monospace,SFMono-Regular,Menlo,monospace;text-decoration:none;letter-spacing:.02em;transition:background-color .15s,border-color .15s}
#gw-console-link:hover{background:#dc2626;color:#ffffff}
#gw-console-dot{width:7px;height:7px;border-radius:999px;background:#dc2626;display:inline-block}
#gw-console-link:hover #gw-console-dot{background:#ffffff}
</style>`

const gatewayNavWidget = `<a id="gw-console-link" href="/gateway" title="Open 1-Gateway System Console"><span id="gw-console-dot"></span><span>GATEWAY CONSOLE</span></a>`

const maxProxiedHTMLBytes = 4 << 20 // 4 MiB safety cap for HTML theme injection

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
		// Request uncompressed HTML on page routes so theme CSS overrides inject cleanly.
		if !strings.HasPrefix(req.URL.Path, "/_next/") {
			req.Header.Set("Accept-Encoding", "identity")
		}
	}
	proxy.ModifyResponse = func(resp *http.Response) error {
		ct := strings.ToLower(resp.Header.Get("Content-Type"))
		if resp.StatusCode != http.StatusOK || !strings.Contains(ct, "text/html") || resp.Header.Get("Content-Encoding") != "" {
			return nil
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProxiedHTMLBytes))
		_ = resp.Body.Close()
		if err != nil {
			return err
		}
		modified := raw
		if bytes.Contains(modified, []byte("</head>")) {
			modified = bytes.Replace(modified, []byte("</head>"), []byte(gatewayThemeStyleTag+"</head>"), 1)
		}
		if bytes.Contains(modified, []byte("</body>")) {
			modified = bytes.Replace(modified, []byte("</body>"), []byte(gatewayNavWidget+"</body>"), 1)
		}
		resp.Body = io.NopCloser(bytes.NewReader(modified))
		resp.ContentLength = int64(len(modified))
		resp.Header.Set("Content-Length", strconv.Itoa(len(modified)))
		return nil
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, proxyErr error) {
		log.Warn("dashboard-proxy", "upstream unavailable", "target", u.String(), "path", r.URL.Path, "error", proxyErr)
		handlerutil.WriteJSONError(w, http.StatusServiceUnavailable, "Dashboard backend is unavailable. Start the companion 9router dashboard or check DASHBOARD_URL.")
	}
	return proxy, nil
}
