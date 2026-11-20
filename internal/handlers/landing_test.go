package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestLandingPortal_PublicOverviewAndSecretRedaction(t *testing.T) {
	r, _, _ := setupGatewayTestRouter(t)

	// 1. Public portal / serves clean HTML without auth and without em dash slop.
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "One endpoint, every active model.") {
		t.Fatalf("expected 200 landing portal HTML, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "\u2014") {
		t.Fatalf("landing_ui.html must not contain em dash characters")
	}

	// 2. Public overview returns live sanitized stats, pools, models, and providers without leaking keys/secrets.
	ovReq := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20128/api/gateway/public-overview", nil)
	ovReq.Host = "127.0.0.1:20128"
	ovRec := httptest.NewRecorder()
	r.ServeHTTP(ovRec, ovReq)
	if ovRec.Code != http.StatusOK {
		t.Fatalf("expected 200 public overview, got %d: %s", ovRec.Code, ovRec.Body.String())
	}
	body := ovRec.Body.String()
	for _, forbidden := range []string{"sk-secret-must-not-leak", "sk-test-gw", "proj-live", "ag-main"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("public overview leaked sensitive value %q: %s", forbidden, body)
		}
	}

	var parsed struct {
		Brand        string              `json:"brand"`
		EndpointV1   string              `json:"endpointV1"`
		Stats        map[string]any      `json:"stats"`
		UnifiedPools map[string][]string `json:"unifiedPools"`
		Models       []publicModelItem   `json:"models"`
		Providers    []publicProviderSummary `json:"providers"`
	}
	if err := json.Unmarshal(ovRec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("decode public overview: %v", err)
	}
	if parsed.Brand != "9Router Gateway" || parsed.EndpointV1 != "http://127.0.0.1:20128/v1" {
		t.Fatalf("unexpected overview metadata: brand=%q endpointV1=%q", parsed.Brand, parsed.EndpointV1)
	}
	if len(parsed.UnifiedPools["auto"]) == 0 || len(parsed.Models) == 0 || len(parsed.Providers) < 2 {
		t.Fatalf("expected active unified pools, models, and providers; got pools=%v models=%d providers=%d", parsed.UnifiedPools, len(parsed.Models), len(parsed.Providers))
	}
}
