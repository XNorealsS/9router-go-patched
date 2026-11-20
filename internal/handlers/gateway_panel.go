package handlers

import (
	_ "embed"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"

	"9router/proxy/internal/autonomy"
	"9router/proxy/internal/db"
	"9router/proxy/internal/handlerutil"
	"9router/proxy/internal/log"
	"9router/proxy/internal/tracing"
	"9router/proxy/internal/updater"
	"9router/proxy/internal/usagetracker"
)

//go:embed gateway_ui.html
var gatewayPanelHTML []byte

// GatewayRuntimeInfo describes the active unified gateway deployment.
type GatewayRuntimeInfo struct {
	StartTime    time.Time
	ListenAddr   string
	DashboardURL string
	DatabasePath string
	DataDir      string
	Engine       *autonomy.Engine
}

// GatewayPanelHandler serves the 1-gateway management UI shell and authenticated
// system management APIs (/api/gateway/*).
type GatewayPanelHandler struct {
	Repo    *db.Repo
	TS      *TokenSaverConfig
	Runtime GatewayRuntimeInfo
}

// NewGatewayPanelHandler initializes a GatewayPanelHandler.
func NewGatewayPanelHandler(repo *db.Repo, ts *TokenSaverConfig, info GatewayRuntimeInfo) *GatewayPanelHandler {
	if info.StartTime.IsZero() {
		info.StartTime = time.Now()
	}
	return &GatewayPanelHandler{Repo: repo, TS: ts, Runtime: info}
}

// HandlePanelPage serves the embedded Crimson/Black/White 1-Gateway system console shell.
// GET /gateway
func HandlePanelPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(gatewayPanelHTML)
}

type gatewayConnSummary struct {
	ID           string   `json:"id"`
	Provider     string   `json:"provider"`
	Name         string   `json:"name"`
	AuthType     string   `json:"authType"`
	IsActive     bool     `json:"isActive"`
	ProjectID    string   `json:"projectId,omitempty"`
	BackoffLevel int      `json:"backoffLevel"`
	ActiveLocks  []string `json:"activeLocks,omitempty"`
}

// HandleSystemStatus returns redacted runtime, encryption, DB, connection lock,
// and telemetry state for the 1-gateway console.
// GET /api/gateway/system
func (h *GatewayPanelHandler) HandleSystemStatus(w http.ResponseWriter, r *http.Request) {
	var dbSize int64
	if h.Runtime.DatabasePath != "" {
		if st, err := os.Stat(h.Runtime.DatabasePath); err == nil {
			dbSize = st.Size()
		}
	}

	var conns []gatewayConnSummary
	if h.Repo != nil {
		if rows, err := h.Repo.GetProviderConnections("", false); err == nil {
			now := time.Now().UTC()
			for _, c := range rows {
				summary := gatewayConnSummary{
					ID:       c.ID,
					Provider: c.Provider,
					AuthType: c.AuthType,
					IsActive: c.IsActive == 1,
				}
				if c.Name != nil {
					summary.Name = *c.Name
				}
				if c.Data != "" {
					var raw map[string]any
					if json.Unmarshal([]byte(c.Data), &raw) == nil {
						if pid, ok := raw["projectId"].(string); ok {
							summary.ProjectID = pid
						}
						if lvl, ok := raw["backoffLevel"].(float64); ok {
							summary.BackoffLevel = int(lvl)
						}
						for k, v := range raw {
							if strings.HasPrefix(k, "modelLock_") {
								if ts, ok := v.(string); ok && ts != "" {
									if exp, err := time.Parse(time.RFC3339, ts); err == nil && now.Before(exp) {
										summary.ActiveLocks = append(summary.ActiveLocks, strings.TrimPrefix(k, "modelLock_"))
									}
								}
							}
						}
					}
				}
				conns = append(conns, summary)
			}
		}
	}

	tsState := map[string]any{}
	if h.TS != nil {
		tsState = map[string]any{
			"rtk":            h.TS.RTKEnabled(),
			"caveman":        h.TS.CavemanEnabled(),
			"cavemanLevel":   h.TS.CavemanLevel(),
			"ponytail":       h.TS.PonytailEnabled(),
			"ponytailLevel":  h.TS.PonytailLevel(),
			"injectionGuard": h.TS.InjectionGuardEnabled(),
		}
	}

	var autoSnap autonomy.SystemStatusSnapshot
	if h.Runtime.Engine != nil {
		autoSnap = h.Runtime.Engine.StatusSnapshot()
	}

	usageSummary := buildGatewayUsageOverview(h.Repo, conns, autoSnap)

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"version":                     updater.CurrentVersion,
		"goVersion":                   runtime.Version(),
		"os":                          runtime.GOOS,
		"arch":                        runtime.GOARCH,
		"uptime":                      time.Since(h.Runtime.StartTime).Round(time.Second).String(),
		"listenAddr":                  h.Runtime.ListenAddr,
		"dashboardUrl":                h.Runtime.DashboardURL,
		"dashboardOnline":             probeHTTPTarget(h.Runtime.DashboardURL),
		"credentialEncryptionEnabled": db.HasCredentialEncryptionKey(),
		"databasePath":                h.Runtime.DatabasePath,
		"databaseSizeBytes":           dbSize,
		"tokenSavers":                 tsState,
		"connections":                 conns,
		"inFlight":                    usagetracker.GetTracker().GetActiveState(h.Repo),
		"latencyStats":                tracing.LatencyByKey(),
		"autonomy":                    autoSnap,
		"usageSummary":                usageSummary,
	})
}

// HandleSyncNow triggers an immediate Antigravity OAuth/project/catalog sync pass.
// POST /api/gateway/sync
func (h *GatewayPanelHandler) HandleSyncNow(w http.ResponseWriter, r *http.Request) {
	if h.Runtime.Engine == nil {
		handlerutil.WriteJSONError(w, http.StatusServiceUnavailable, "autonomy engine unavailable")
		return
	}
	h.Runtime.Engine.SyncNow(r.Context(), "manual")
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"autonomy": h.Runtime.Engine.StatusSnapshot(),
	})
}

// HandleToggles updates runtime token saver and prompt-injection guard toggles.
// POST /api/gateway/toggles
func (h *GatewayPanelHandler) HandleToggles(w http.ResponseWriter, r *http.Request) {
	if h.TS == nil {
		handlerutil.WriteJSONError(w, http.StatusServiceUnavailable, "token saver config unavailable")
		return
	}
	var req struct {
		RTK            *bool   `json:"rtk"`
		Caveman        *bool   `json:"caveman"`
		CavemanLevel   string  `json:"cavemanLevel"`
		Ponytail       *bool   `json:"ponytail"`
		PonytailLevel  string  `json:"ponytailLevel"`
		InjectionGuard *bool   `json:"injectionGuard"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.RTK != nil {
		h.TS.SetRTK(*req.RTK)
	}
	if req.Caveman != nil || req.CavemanLevel != "" {
		enabled := h.TS.CavemanEnabled()
		if req.Caveman != nil {
			enabled = *req.Caveman
		}
		if req.CavemanLevel != "" {
			h.TS.SetCaveman(enabled, req.CavemanLevel)
		} else {
			h.TS.SetCaveman(enabled)
		}
	}
	if req.Ponytail != nil || req.PonytailLevel != "" {
		enabled := h.TS.PonytailEnabled()
		if req.Ponytail != nil {
			enabled = *req.Ponytail
		}
		if req.PonytailLevel != "" {
			h.TS.SetPonytail(enabled, req.PonytailLevel)
		} else {
			h.TS.SetPonytail(enabled)
		}
	}
	if req.InjectionGuard != nil {
		h.TS.SetInjectionGuard(*req.InjectionGuard)
	}
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"rtk":            h.TS.RTKEnabled(),
		"caveman":        h.TS.CavemanEnabled(),
		"cavemanLevel":   h.TS.CavemanLevel(),
		"ponytail":       h.TS.PonytailEnabled(),
		"ponytailLevel":  h.TS.PonytailLevel(),
		"injectionGuard": h.TS.InjectionGuardEnabled(),
	})
}

// HandleHealthReset clears model cooldown locks globally or for a provider/model.
// POST /api/gateway/health/reset
func (h *GatewayPanelHandler) HandleHealthReset(w http.ResponseWriter, r *http.Request) {
	if h.Repo == nil {
		handlerutil.WriteJSONError(w, http.StatusServiceUnavailable, "repository unavailable")
		return
	}
	var req struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := h.Repo.ResetProviderHealth(req.Provider, req.Model); err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	handlerutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// HandleUpdateCheck runs a live release manifest check cleanly.
// POST /api/gateway/update-check
func (h *GatewayPanelHandler) HandleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	info, err := updater.CheckUpdate(r.Context())
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	handlerutil.WriteJSON(w, http.StatusOK, info)
}

// HandleLogsGet returns buffered gateway console logs.
func (h *GatewayPanelHandler) HandleLogsGet(w http.ResponseWriter, r *http.Request) {
	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"logs": log.ConsoleLogs(),
	})
}

// HandleLogsDelete clears buffered gateway console logs.
func (h *GatewayPanelHandler) HandleLogsDelete(w http.ResponseWriter, r *http.Request) {
	log.ClearConsoleLogs()
	handlerutil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func probeHTTPTarget(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		if u.Scheme == "https" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	c, err := net.DialTimeout("tcp", host, 350*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

type gatewayProviderUsage struct {
	Provider          string   `json:"provider"`
	AuthType          string   `json:"authType"`
	Accounts          int      `json:"accounts"`
	LimitPolicy       string   `json:"limitPolicy"`
	QuotaRemainingPct *int     `json:"quotaRemainingPct,omitempty"`
	QuotaResetTime    string   `json:"quotaResetTime,omitempty"`
	Requests24h       int      `json:"requests24h"`
	TotalRequests     int      `json:"totalRequests"`
	PromptTokens      int      `json:"promptTokens"`
	CompletionTokens  int      `json:"completionTokens"`
	TotalTokens       int      `json:"totalTokens"`
	Tokens24h         int      `json:"tokens24h"`
	BackoffLevel      int      `json:"backoffLevel"`
	ActiveLocks       []string `json:"activeLocks,omitempty"`
	LastUsedAt        string   `json:"lastUsedAt,omitempty"`
}

type gatewayModelUsage struct {
	Provider          string `json:"provider"`
	Model             string `json:"model"`
	Requests          int    `json:"requests"`
	PromptTokens      int    `json:"promptTokens"`
	CompletionTokens  int    `json:"completionTokens"`
	TotalTokens       int    `json:"totalTokens"`
	QuotaRemainingPct *int   `json:"quotaRemainingPct,omitempty"`
	QuotaResetTime    string `json:"quotaResetTime,omitempty"`
	LastUsedAt        string `json:"lastUsedAt,omitempty"`
}

type gatewayUsageOverview struct {
	TotalRequests    int                    `json:"totalRequests"`
	Requests24h      int                    `json:"requests24h"`
	PromptTokens     int                    `json:"promptTokens"`
	CompletionTokens int                    `json:"completionTokens"`
	TotalTokens      int                    `json:"totalTokens"`
	Tokens24h        int                    `json:"tokens24h"`
	EstimatedCostUSD float64                `json:"estimatedCostUsd"`
	ByProvider       []gatewayProviderUsage `json:"byProvider"`
	ByModel          []gatewayModelUsage    `json:"byModel"`
}

func providerLimitPolicy(provider string) (authDefault, policy string) {
	switch provider {
	case "antigravity":
		return "oauth", "Cloud Code daily per-model quota (0-100% live) + 1h OAuth auto-refresh"
	case "deepseek-web":
		return "cookie", "Free Web Cookie + Keccak PoW · chat.deepseek.com session rate/fair-use limit"
	case "kilo-gateway":
		return "no-auth", "Zero-Config Free Tier ($0/token) · per-IP RPM & model context window cap"
	case "openrouter":
		return "apikey", "Free (:free): 20 RPM & 50-1000 req/day · Paid (r1-1776): USD credit balance"
	case "venice":
		return "apikey", "Venice API tier RPM/TPM & credit/DIEM balance (venice-uncensored / R1-671B)"
	case "perplexity":
		return "apikey", "Sonar API credit balance & tier RPM (r1-1776 uncensored DeepSeek-R1)"
	case "nvidia", "chutes":
		return "apikey", "Free/API trial credits & per-key RPM rate limit"
	case "ollama":
		return "local", "Local hardware RAM/VRAM & concurrency only (no external token cap)"
	default:
		return "apikey", "Upstream provider API key quota & RPM/TPM rate limits"
	}
}

func buildGatewayUsageOverview(repo *db.Repo, conns []gatewayConnSummary, autoSnap autonomy.SystemStatusSnapshot) gatewayUsageOverview {
	out := gatewayUsageOverview{
		ByProvider: []gatewayProviderUsage{},
		ByModel:    []gatewayModelUsage{},
	}
	provMap := make(map[string]*gatewayProviderUsage)
	ensureProv := func(p string) *gatewayProviderUsage {
		if existing, ok := provMap[p]; ok {
			return existing
		}
		authDef, pol := providerLimitPolicy(p)
		item := &gatewayProviderUsage{
			Provider:    p,
			AuthType:    authDef,
			LimitPolicy: pol,
		}
		provMap[p] = item
		return item
	}

	// Seed active/configured connections and built-in zero-config free pool.
	for _, c := range conns {
		if !c.IsActive {
			continue
		}
		pu := ensureProv(c.Provider)
		if c.AuthType != "" {
			pu.AuthType = c.AuthType
		}
		pu.Accounts++
		if c.BackoffLevel > pu.BackoffLevel {
			pu.BackoffLevel = c.BackoffLevel
		}
		for _, lk := range c.ActiveLocks {
			pu.ActiveLocks = append(pu.ActiveLocks, lk)
		}
	}
	kilo := ensureProv("kilo-gateway")
	if kilo.Accounts == 0 {
		kilo.Accounts = 1
	}

	// Attach live Antigravity quota metadata from CatalogSnapshot.
	modelQuotaPct := make(map[string]int)
	modelQuotaReset := make(map[string]string)
	minAGQuota := 101
	var agReset string
	for _, m := range autoSnap.Catalog.Models {
		if m.QuotaRemaining != nil {
			pct := int(*m.QuotaRemaining*100 + 0.5)
			key := (m.Provider + "/" + m.ID)
			modelQuotaPct[key] = pct
			modelQuotaReset[key] = m.QuotaResetTime
			if (m.Provider == "" || m.Provider == "antigravity") && pct < minAGQuota {
				minAGQuota = pct
				agReset = m.QuotaResetTime
			}
		}
	}
	if minAGQuota <= 100 {
		ag := ensureProv("antigravity")
		v := minAGQuota
		ag.QuotaRemainingPct = &v
		ag.QuotaResetTime = agReset
	}

	if repo != nil && repo.RawDB() != nil {
		cutoff24h := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
		rows, err := repo.RawDB().Query(`
			SELECT
				COALESCE(provider, 'unknown'),
				COALESCE(model, 'unknown'),
				COUNT(*),
				SUM(CASE WHEN timestamp >= ? THEN 1 ELSE 0 END),
				COALESCE(SUM(promptTokens), 0),
				COALESCE(SUM(completionTokens), 0),
				COALESCE(SUM(CASE WHEN timestamp >= ? THEN COALESCE(promptTokens,0)+COALESCE(completionTokens,0) ELSE 0 END), 0),
				COALESCE(SUM(cost), 0),
				COALESCE(MAX(timestamp), '')
			FROM usageHistory
			GROUP BY provider, model
			ORDER BY COUNT(*) DESC, (COALESCE(SUM(promptTokens),0)+COALESCE(SUM(completionTokens),0)) DESC
		`, cutoff24h, cutoff24h)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var prov, mod, lastTS string
				var reqTotal, req24, pTok, cTok, tok24 int
				var cost float64
				if err := rows.Scan(&prov, &mod, &reqTotal, &req24, &pTok, &cTok, &tok24, &cost, &lastTS); err != nil {
					continue
				}
				totTok := pTok + cTok
				out.TotalRequests += reqTotal
				out.Requests24h += req24
				out.PromptTokens += pTok
				out.CompletionTokens += cTok
				out.TotalTokens += totTok
				out.Tokens24h += tok24
				out.EstimatedCostUSD += cost

				pu := ensureProv(prov)
				pu.TotalRequests += reqTotal
				pu.Requests24h += req24
				pu.PromptTokens += pTok
				pu.CompletionTokens += cTok
				pu.TotalTokens += totTok
				pu.Tokens24h += tok24
				if lastTS > pu.LastUsedAt {
					pu.LastUsedAt = lastTS
				}

				mu := gatewayModelUsage{
					Provider:         prov,
					Model:            mod,
					Requests:         reqTotal,
					PromptTokens:     pTok,
					CompletionTokens: cTok,
					TotalTokens:      totTok,
					LastUsedAt:       lastTS,
				}
				if pct, ok := modelQuotaPct[prov+"/"+mod]; ok {
					v := pct
					mu.QuotaRemainingPct = &v
					mu.QuotaResetTime = modelQuotaReset[prov+"/"+mod]
				}
				out.ByModel = append(out.ByModel, mu)
			}
		}
	}

	for _, pu := range provMap {
		out.ByProvider = append(out.ByProvider, *pu)
	}
	sort.Slice(out.ByProvider, func(i, j int) bool {
		if out.ByProvider[i].TotalRequests != out.ByProvider[j].TotalRequests {
			return out.ByProvider[i].TotalRequests > out.ByProvider[j].TotalRequests
		}
		return out.ByProvider[i].Provider < out.ByProvider[j].Provider
	})
	return out
}
