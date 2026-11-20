package autonomy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"9router/proxy/internal/db"
	"9router/proxy/internal/dbtest"
	"9router/proxy/internal/translator"
)

func TestCatalogSync_ParsesCapabilitiesDiffsAndAliases(t *testing.T) {
	rec := NewRecorder()
	cat := NewCatalogManager(rec)

	var callCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-live" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n := callCount.Add(1)
		models := map[string]any{
			"gemini-3-flash": map[string]any{
				"displayName":       "Gemini 3 Flash",
				"maxTokens":         1048576,
				"maxOutputTokens":   65536,
				"minThinkingBudget": 128,
				"recommended":       true,
				"supportedMimeTypes": map[string]bool{
					"image/png":       true,
					"application/pdf": true,
				},
				"quotaInfo": map[string]any{
					"remainingFraction": 0.92,
					"resetTime":         "2026-11-20T12:00:00Z",
				},
			},
			"gemini-3.8-flash-medium": map[string]any{
				"displayName":       "Gemini 3.8 Flash",
				"maxTokens":         1048576,
				"maxOutputTokens":   65536,
				"minThinkingBudget": 128,
				"supportedMimeTypes": map[string]bool{
					"image/jpeg":      true,
					"application/pdf": true,
				},
			},
		}
		if n == 2 {
			// Second sync adds one model and removes gemini-3.8-flash-medium
			delete(models, "gemini-3.8-flash-medium")
			models["gemini-4-hydrogen-high"] = map[string]any{
				"displayName":       "Gemini 4 Hydrogen (High)",
				"maxTokens":         1048576,
				"maxOutputTokens":   131072,
				"minThinkingBudget": 32,
				"recommended":       true,
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"defaultAgentModelId": "gemini-3-flash",
			"models":              models,
		})
	}))
	defer srv.Close()
	cat.modelsURL = srv.URL

	diff1, err := cat.SyncAntigravity(context.Background(), srv.Client(), "tok-live", "proj-1")
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if diff1.VerifiedModelCount != 2 {
		t.Fatalf("expected 2 verified models, got %d", diff1.VerifiedModelCount)
	}
	caps := cat.CapabilitiesFor("antigravity", "gemini-3-flash")
	if !caps.Vision || !caps.PDF || !caps.Reasoning || !caps.Tools {
		t.Fatalf("unexpected parsed capabilities: %+v", caps)
	}
	if q := cat.QuotaRemaining("gemini-3-flash"); q < 0.91 || q > 0.93 {
		t.Fatalf("expected quota ~0.92, got %v", q)
	}

	// Second sync compares added/removed and marks missing synonym targets deprecated.
	diff2, err := cat.SyncAntigravity(context.Background(), srv.Client(), "tok-live", "proj-1")
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(diff2.Added) != 1 || diff2.Added[0] != "gemini-4-hydrogen-high" {
		t.Fatalf("expected added gemini-4-hydrogen-high, got %v", diff2.Added)
	}
	if len(diff2.Removed) != 1 || diff2.Removed[0] != "gemini-3.8-flash-medium" {
		t.Fatalf("expected removed gemini-3.8-flash-medium, got %v", diff2.Removed)
	}

	// Resolving an obsolete synonym mapped to removed model auto-routes to verified fallback.
	if target := translator.NormalizeAntigravityModel("gemini-3.5-flash"); target != "" {
		resolved, changed, reason := cat.ResolveHealthyModel("gemini-3.5-flash")
		if target == "gemini-3.8-flash-medium" && (!changed || resolved == target || reason == "") {
			t.Fatalf("expected healthy fallback away from deprecated %s, got %s changed=%v reason=%s", target, resolved, changed, reason)
		}
	}
}

func TestPlanRecovery_DistinguishesTransientVsAdminAndSchemaErrors(t *testing.T) {
	revoked := PlanRecovery("antigravity", "gemini-3-flash", 401, "invalid_grant: Token has been expired or revoked.", 0)
	if !revoked.NeedsAdmin || revoked.Strategy != StrategyAdminRequired {
		t.Fatalf("expected revoked token to require admin, got %+v", revoked)
	}

	schemaErr := PlanRecovery("antigravity", "gemini-3-flash", 400, "INVALID_ARGUMENT in function_declarations", 0)
	if schemaErr.ShouldFallback || schemaErr.Strategy != StrategyAbortNoRetry {
		t.Fatalf("expected bad tool schema 400 to abort without cross-account retry, got %+v", schemaErr)
	}

	dep := PlanRecovery("antigravity", "gemini-pro-agent", 404, "model not_found", 0)
	if !dep.MarkModelDeprecated || dep.Strategy != StrategyDeprecateFallback {
		t.Fatalf("expected 404 model to trigger deprecation fallback, got %+v", dep)
	}

	rate := PlanRecovery("antigravity", "gemini-3-flash", 429, "RESOURCE_EXHAUSTED quota exceeded", 1)
	if !rate.ShouldRotateAccount || rate.NewBackoffLevel != 2 || rate.CooldownSec <= 0 {
		t.Fatalf("expected 429 exponential backoff + rotation, got %+v", rate)
	}
}

func TestRecorder_SessionsPrivacyAndPendingAdmin(t *testing.T) {
	rec := NewRecorder()
	key1 := DeriveSessionKey("sk-client-1", "antigravity", "secret user prompt must never appear")
	key2 := DeriveSessionKey("sk-client-1", "antigravity", "secret user prompt must never appear")
	if key1 != key2 || !startsWith(key1, "sess_") {
		t.Fatalf("unexpected deterministic session key: %s vs %s", key1, key2)
	}

	rec.TrackSession(key1, "antigravity", "conn-a", "gemini-3-flash", 2, 0, true, "200")
	rec.TrackSession(key1, "antigravity", "conn-a", "gemini-3-flash", 0, 2, true, "200")
	if got := rec.GetSessionConnection(key1); got != "conn-a" {
		t.Fatalf("expected sticky conn-a, got %s", got)
	}
	sessions := rec.ActiveSessions(10)
	if len(sessions) != 1 || sessions[0].Turns != 2 || sessions[0].ToolCalls != 2 || !sessions[0].HasThoughtSig {
		t.Fatalf("unexpected session tracking: %+v", sessions)
	}

	// Admin alert clears automatically when a subsequent OK event succeeds in same scope.
	rec.Record(Event{Category: CategoryOAuth, Provider: "antigravity", ConnectionID: "conn-a", Detected: "expired", Action: "refresh", Reason: "err", Outcome: OutcomeNeedsAdmin, NeedsAdmin: true})
	if pending := rec.PendingAdminEvents(); len(pending) != 1 {
		t.Fatalf("expected 1 pending admin event, got %d", len(pending))
	}
	rec.Record(Event{Category: CategoryOAuth, Provider: "antigravity", ConnectionID: "conn-a", Detected: "recovered", Action: "refresh", Reason: "ok", Outcome: OutcomeOK})
	if pending := rec.PendingAdminEvents(); len(pending) != 0 {
		t.Fatalf("expected pending admin cleared after recovery, got %d", len(pending))
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func TestEngine_FullSyncLifecycle(t *testing.T) {
	database, err := db.OpenDatabase(filepath.Join(t.TempDir(), "test.sqlite"))
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	for _, stmt := range dbtest.SchemaStatements() {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	expiredAt := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	_, err = database.Exec(
		`INSERT INTO providerConnections (id, provider, authType, name, isActive, data, createdAt, updatedAt) VALUES ('c1', 'antigravity', 'oauth', 'ag-main', 1, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
		`{"accessToken":"old-tok","refreshToken":"rt-1","expiresAt":"`+expiredAt+`"}`,
	)
	if err != nil {
		t.Fatalf("seed conn: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/kilo" {
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"defaultAgentModelId": "gemini-3-flash",
			"models": map[string]any{
				"gemini-3-flash": map[string]any{"displayName": "Gemini 3 Flash", "maxTokens": 1048576, "maxOutputTokens": 65536},
			},
		})
	}))
	defer srv.Close()
	t.Setenv("KILO_GATEWAY_MODELS_URL", srv.URL+"/kilo")

	repo := db.NewRepo(database)
	eng := NewEngine(repo, srv.Client())
	eng.Catalog.modelsURL = srv.URL

	var refreshed, discovered atomic.Int32
	eng.RefreshFn = func(ctx context.Context, connID, currentToken string) (string, string, error) {
		refreshed.Add(1)
		return "fresh-tok", "", nil
	}
	eng.DiscoverFn = func(ctx context.Context, connID, accessToken string) (string, error) {
		discovered.Add(1)
		if accessToken != "fresh-tok" {
			t.Fatalf("expected fresh-tok in project discovery, got %s", accessToken)
		}
		return "proj-discovered", nil
	}

	eng.SyncNow(context.Background(), "startup")
	if refreshed.Load() != 1 || discovered.Load() != 1 {
		t.Fatalf("expected 1 refresh & 1 project discovery, got refresh=%d discover=%d", refreshed.Load(), discovered.Load())
	}

	snap := eng.StatusSnapshot()
	if snap.OverallHealth != "autonomous" || !snap.Catalog.Synced || snap.Catalog.VerifiedCount != 1 {
		t.Fatalf("unexpected engine status snapshot: %+v", snap)
	}
}

func TestSyncKiloFree_AndResolveUnifiedPool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "kilo-auto/free", "name": "Kilo Auto Free", "context_length": 200000, "max_completion_tokens": 16384},
				{"id": "nvidia/nemotron-3.5-super-vl:free", "name": "Nemotron VL", "context_length": 131072, "max_completion_tokens": 8192, "architecture": map[string]any{"input_modalities": []string{"text", "image"}}},
				{"id": "paid/gpt-pro", "name": "Paid Model", "context_length": 128000, "pricing": map[string]any{"prompt": "0.005"}},
			},
		})
	}))
	defer srv.Close()
	t.Setenv("KILO_GATEWAY_MODELS_URL", srv.URL)

	database, err := db.OpenDatabase(filepath.Join(t.TempDir(), "unify.sqlite"))
	if err != nil {
		t.Fatalf("OpenDatabase: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	for _, stmt := range dbtest.SchemaStatements() {
		if _, err := database.Exec(stmt); err != nil {
			t.Fatalf("schema: %v", err)
		}
	}
	_, err = database.Exec(
		`INSERT INTO providerConnections (id, provider, authType, name, isActive, data, createdAt, updatedAt) VALUES ('d1', 'deepseek-web', 'cookie', 'ds-cookie', 1, '{"apiKey":"tok"}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'), ('a1', 'antigravity', 'oauth', 'ag-main', 1, '{"accessToken":"t","projectId":"p"}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	)
	if err != nil {
		t.Fatalf("seed conns: %v", err)
	}

	repo := db.NewRepo(database)
	cat := NewCatalogManager(NewRecorder())
	count, err := cat.SyncKiloFree(context.Background(), srv.Client())
	if err != nil || count != 2 {
		t.Fatalf("expected 2 free models synced, got %d err=%v", count, err)
	}
	if ctxWin, _ := cat.ModelLimits("kilo-gateway", "kilo-auto/free"); ctxWin != 200000 {
		t.Fatalf("expected synced 200k context limit, got %d", ctxWin)
	}

	// Unified bare model names expand across active providers cleanly.
	dsPool := ResolveUnifiedPool(repo, cat, "deepseek-chat")
	if len(dsPool) < 2 || dsPool[0] != "deepseek-web/deepseek-chat" {
		t.Fatalf("unexpected unified deepseek-chat pool: %v", dsPool)
	}
	claudePool := ResolveUnifiedPool(repo, cat, "claude-sonnet-4-6")
	if len(claudePool) < 2 || claudePool[0] != "antigravity/claude-sonnet-5-5-medium" {
		t.Fatalf("unexpected unified claude pool: %v", claudePool)
	}
	autoPool := ResolveUnifiedPool(repo, cat, "auto")
	if len(autoPool) < 4 {
		t.Fatalf("expected unified auto pool combining active+free providers, got %v", autoPool)
	}

	// Uncensored pool stays inactive until OpenRouter / Venice / Perplexity is connected.
	if uncBefore := ResolveUnifiedPool(repo, cat, "deepseek-uncensored"); len(uncBefore) != 0 {
		t.Fatalf("expected deepseek-uncensored pool empty before provider connected, got %v", uncBefore)
	}
	_, err = database.Exec(
		`INSERT INTO providerConnections (id, provider, authType, name, isActive, data, createdAt, updatedAt) VALUES ('p1', 'perplexity', 'apikey', 'pplx', 1, '{"apiKey":"k"}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'), ('or1', 'openrouter', 'apikey', 'or', 1, '{"apiKey":"k"}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z'), ('v1', 'venice', 'apikey', 'vn', 1, '{"apiKey":"k"}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
	)
	if err != nil {
		t.Fatalf("seed uncensored conns: %v", err)
	}
	uncAfter := ResolveUnifiedPool(repo, cat, "deepseek-uncensored")
	if len(uncAfter) != 5 || uncAfter[0] != "perplexity/r1-1776" || uncAfter[1] != "openrouter/perplexity/r1-1776" || uncAfter[2] != "venice/venice-uncensored" {
		t.Fatalf("unexpected deepseek-uncensored pool after connecting providers: %v", uncAfter)
	}
}
