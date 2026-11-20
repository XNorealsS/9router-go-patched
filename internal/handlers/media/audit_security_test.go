package media

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"9router/proxy/internal/db"
)

func TestHandleHeadroomProxy_BlocksSSRFMetadataURL(t *testing.T) {
	database, cleanup := setupResponsesTestDB(t)
	defer cleanup()

	if _, err := database.Exec(`INSERT INTO settings (id, data) VALUES (1, '{"headroomUrl":"http://169.254.169.254/latest/meta-data"}')`); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	h := NewHeadroomHandler(db.NewRepo(database))
	req := httptest.NewRequest(http.MethodGet, "/headroom/proxy/test", nil)
	rec := httptest.NewRecorder()
	h.HandleHeadroomProxy(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 SSRF_BLOCKED, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBuildMediaTargetURL_StripsChatCompletions(t *testing.T) {
	got := buildMediaTargetURL("https://api.openai.com/v1/chat/completions", "/responses")
	if got != "https://api.openai.com/v1/responses" {
		t.Errorf("expected https://api.openai.com/v1/responses, got %s", got)
	}
	gotV1 := buildMediaTargetURL("https://api.openai.com/v1/chat/completions", "/v1/images/generations")
	if gotV1 != "https://api.openai.com/v1/images/generations" {
		t.Errorf("expected https://api.openai.com/v1/images/generations, got %s", gotV1)
	}
}

func TestCopySanitizedMediaReqHeaders_StripsSensitiveHeaders(t *testing.T) {
	src := http.Header{
		"Authorization": []string{"Bearer client-secret"},
		"X-Api-Key":     []string{"client-api-key"},
		"Cookie":        []string{"session=abc"},
		"Content-Type":  []string{"application/json"},
	}
	dst := http.Header{}
	copySanitizedMediaReqHeaders(dst, src)
	if dst.Get("Authorization") != "" || dst.Get("X-Api-Key") != "" || dst.Get("Cookie") != "" {
		t.Errorf("sensitive client headers leaked: %v", dst)
	}
	if dst.Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type preserved, got %q", dst.Get("Content-Type"))
	}
}

func TestRenderEdgeRelayCode_RequiresSecretAndBlocksPrivateTargets(t *testing.T) {
	for _, code := range []string{
		renderVercelRelayCode("test-secret-123"),
		renderCloudflareRelayCode("test-secret-123"),
		renderDenoRelayCode("test-secret-123"),
	} {
		if !strings.Contains(code, "x-relay-secret") || !strings.Contains(code, "isAllowedRelayTarget") {
			t.Errorf("relay template missing auth or SSRF guard")
		}
	}
}
