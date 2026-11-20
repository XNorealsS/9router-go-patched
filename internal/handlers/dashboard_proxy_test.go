package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"9router/proxy/internal/db"
)

func TestSetupServerRouterWithGateway_UnifiedGateway(t *testing.T) {
	dash := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dashboard":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html><head></head><body><div>9Router Dashboard</div></body></html>"))
		case "/api/version":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"source":"nextjs-dashboard"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer dash.Close()

	dashProxy, err := NewDashboardProxy(dash.URL)
	if err != nil {
		t.Fatalf("NewDashboardProxy: %v", err)
	}

	database, cleanup := setupTestDB(t)
	defer cleanup()
	repo := db.NewRepo(database)

	r := chi.NewRouter()
	SetupServerRouterWithGateway(r, repo, NewTokenSaverConfig(false, false, false), dashProxy, GatewayRuntimeInfo{})

	// 1. Go /health stays served by Go directly.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("expected Go /health 200, got %d %s", rec.Code, rec.Body.String())
	}

	// 2. Dashboard HTML route falls through to DASHBOARD_URL and receives Red/Black/White theme injection.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "9Router Dashboard") {
		t.Fatalf("expected proxied /dashboard 200, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="gw-red-theme"`) || !strings.Contains(rec.Body.String(), `href="/gateway"`) || !strings.Contains(rec.Body.String(), `gw-sidebar-link`) {
		t.Fatalf("expected red theme override, sidebar link script, and /gateway nav link injected, got %s", rec.Body.String())
	}

	// 3. Browser call to /api/version without API key goes to dashboard instead of 401.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "nextjs-dashboard") {
		t.Fatalf("expected unauthenticated browser /api/version proxied to dashboard, got %d %s", rec.Code, rec.Body.String())
	}

	// 4. Protected LLM /models route without API key STILL enforces 401.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected /models without key to return 401, got %d", rec.Code)
	}
}
