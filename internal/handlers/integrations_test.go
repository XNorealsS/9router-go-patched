package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayIntegrations_HermesClaudeOpenCodeCodex(t *testing.T) {
	r, _, _ := setupGatewayTestRouter(t)

	hermesDir := filepath.Join(t.TempDir(), "hermes")
	t.Setenv("HERMES_HOME", hermesDir)
	claudeDir := filepath.Join(t.TempDir(), "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	xdgDir := filepath.Join(t.TempDir(), "xdg")
	t.Setenv("XDG_CONFIG_HOME", xdgDir)
	codexDir := filepath.Join(t.TempDir(), "codex")
	t.Setenv("CODEX_HOME", codexDir)

	// Seed an initial Hermes config.yaml mimicking v0.22.5 defaults
	if err := os.MkdirAll(hermesDir, 0700); err != nil {
		t.Fatal(err)
	}
	origHermes := "model:\n  default: \"anthropic/claude-opus-4.6\"\n  provider: \"auto\"\n  base_url: \"https://openrouter.ai/api/v1\"\n"
	if err := os.WriteFile(filepath.Join(hermesDir, "config.yaml"), []byte(origHermes), 0600); err != nil {
		t.Fatal(err)
	}

	// 1. GET /api/gateway/integrations lists tools cleanly with redacted key
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:20128/api/gateway/integrations", nil)
	req.Host = "127.0.0.1:20128"
	req.Header.Set("X-API-Key", "sk-test-gw")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Hermes Agent") || !strings.Contains(rec.Body.String(), "127.0.0.1:20128/v1") {
		t.Fatalf("unexpected integrations GET response (%d): %s", rec.Code, rec.Body.String())
	}

	// 2. Apply to all 4 tools cleanly
	for _, tc := range []struct {
		Tool  string `json:"tool"`
		Model string `json:"model"`
	}{
		{"hermes", "auto"},
		{"claude", "claude-sonnet-4-6"},
		{"opencode", "free-pool"},
		{"codex", "deepseek-chat"},
	} {
		body, _ := json.Marshal(tc)
		apReq := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:20128/api/gateway/integrations/apply", bytes.NewReader(body))
		apReq.Host = "127.0.0.1:20128"
		apReq.Header.Set("X-API-Key", "sk-test-gw")
		apRec := httptest.NewRecorder()
		r.ServeHTTP(apRec, apReq)
		if apRec.Code != http.StatusOK {
			t.Fatalf("apply %s failed (%d): %s", tc.Tool, apRec.Code, apRec.Body.String())
		}
	}

	// Verify Hermes config.yaml + .env + backup
	cfgBytes, _ := os.ReadFile(filepath.Join(hermesDir, "config.yaml"))
	cfgStr := string(cfgBytes)
	if !strings.Contains(cfgStr, `default: "auto"`) || !strings.Contains(cfgStr, `provider: "custom"`) || !strings.Contains(cfgStr, `base_url: "http://127.0.0.1:20128/v1"`) {
		t.Fatalf("unexpected updated Hermes config.yaml:\n%s", cfgStr)
	}
	if bak, err := os.ReadFile(filepath.Join(hermesDir, "config.yaml.9router.bak")); err != nil || !strings.Contains(string(bak), "openrouter.ai") {
		t.Fatalf("expected original Hermes config backed up, got err=%v", err)
	}
	if envBytes, err := os.ReadFile(filepath.Join(hermesDir, ".env")); err != nil || !strings.Contains(string(envBytes), "OPENAI_API_KEY=sk-test-gw") {
		t.Fatalf("expected OPENAI_API_KEY written to Hermes .env, got err=%v", err)
	}

	// Verify Claude, OpenCode, Codex written cleanly
	if b, err := os.ReadFile(filepath.Join(claudeDir, "settings.json")); err != nil || !strings.Contains(string(b), "ANTHROPIC_BASE_URL") {
		t.Fatalf("expected Claude settings.json updated, err=%v", err)
	}
	if b, err := os.ReadFile(filepath.Join(xdgDir, "opencode", "opencode.json")); err != nil || !strings.Contains(string(b), "http://127.0.0.1:20128/v1") {
		t.Fatalf("expected OpenCode config updated, err=%v", err)
	}
	if b, err := os.ReadFile(filepath.Join(codexDir, "config.toml")); err != nil || !strings.Contains(string(b), "[model_providers.9router]") {
		t.Fatalf("expected Codex config.toml updated, err=%v", err)
	}
}
