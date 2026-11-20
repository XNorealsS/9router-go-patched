package handlers

import (
	_ "embed"
	"net/http"
	"sort"
	"strings"
	"time"

	"9router/proxy/internal/autonomy"
	"9router/proxy/internal/db"
	"9router/proxy/internal/providers"
	"9router/proxy/internal/updater"
	"9router/proxy/internal/handlerutil"
)

//go:embed landing_ui.html
var landingPortalHTML []byte

// HandleLandingPage serves the public 9Router Unified 1-Gateway landing & live portal at GET /.
func HandleLandingPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(landingPortalHTML)
}

type publicProviderSummary struct {
	Provider    string `json:"provider"`
	AuthType    string `json:"authType"`
	Accounts    int    `json:"accounts"`
	Status      string `json:"status"`
	ModelCount  int    `json:"modelCount"`
	QuotaPct    *int   `json:"quotaRemainingPct,omitempty"`
	LimitPolicy string `json:"limitPolicy"`
}

type publicModelItem struct {
	ID            string                 `json:"id"`
	Provider      string                 `json:"provider"`
	Tier          string                 `json:"tier"`
	ContextLength int                    `json:"contextLength"`
	MaxOutput     int                    `json:"maxOutput"`
	QuotaPct      *int                   `json:"quotaRemainingPct,omitempty"`
	Capabilities  providers.Capabilities `json:"capabilities"`
}

// HandlePublicOverview returns strictly sanitized, credential-free gateway state,
// live model catalog, active failover routes, and 24h aggregate telemetry for GET /.
// GET /api/gateway/public-overview
func (h *GatewayPanelHandler) HandlePublicOverview(w http.ResponseWriter, r *http.Request) {
	baseRoot := resolveGatewayBaseURL(r, h.Runtime.ListenAddr)
	baseV1 := baseRoot + "/v1"

	var autoSnap autonomy.SystemStatusSnapshot
	var cat *autonomy.CatalogManager
	if h.Runtime.Engine != nil {
		cat = h.Runtime.Engine.Catalog
		autoSnap = h.Runtime.Engine.StatusSnapshot()
	}

	var conns []gatewayConnSummary
	activeProvCounts := make(map[string]int)
	activeProvAuth := make(map[string]string)
	if h.Repo != nil {
		if rows, err := h.Repo.GetProviderConnections("", true); err == nil {
			for _, c := range rows {
				if c.IsActive != 1 {
					continue
				}
				activeProvCounts[c.Provider]++
				if activeProvAuth[c.Provider] == "" {
					activeProvAuth[c.Provider] = c.AuthType
				}
				conns = append(conns, gatewayConnSummary{
					ID:       c.ID,
					Provider: c.Provider,
					AuthType: c.AuthType,
					IsActive: true,
				})
			}
		}
	}

	usage := buildGatewayUsageOverview(h.Repo, conns, autoSnap)

	seenModels := make(map[string]bool)
	var models []publicModelItem
	provModelCounts := make(map[string]int)

	addModel := func(id, prov, tier string, ctxLen, maxOut int, quotaPct *int, caps providers.Capabilities) {
		key := prov + "::" + id
		if seenModels[key] {
			return
		}
		seenModels[key] = true
		if ctxLen <= 0 || maxOut <= 0 {
			c2, m2 := providers.GetModelTokenLimits(id)
			if ctxLen <= 0 {
				ctxLen = c2
			}
			if maxOut <= 0 {
				maxOut = m2
			}
		}
		models = append(models, publicModelItem{
			ID:            id,
			Provider:      prov,
			Tier:          tier,
			ContextLength: ctxLen,
			MaxOutput:     maxOut,
			QuotaPct:      quotaPct,
			Capabilities:  caps,
		})
		provModelCounts[prov]++
	}

	// 1. Unified virtual & family pools active on this gateway
	unifiedPools := make(map[string][]string)
	poolNames := append([]string{}, autonomy.UnifiedVirtualModels()...)
	poolNames = append(poolNames, "free-pool", "deepseek-uncensored")
	for _, pName := range poolNames {
		targets := autonomy.ResolveUnifiedPool(h.Repo, cat, pName)
		if len(targets) > 0 {
			unifiedPools[pName] = targets
			tier := "unified"
			if pName == "deepseek-uncensored" {
				tier = "uncensored"
			}
			addModel(pName, "9router-unified", tier, 0, 0, nil, providers.Capabilities{Tools: true, Reasoning: strings.Contains(pName, "reason") || pName == "deepseek-uncensored", Vision: strings.Contains(pName, "vision") || pName == "auto"})
		}
	}

	// 2. Verified live catalog models (Antigravity + Kilo Free)
	if cat != nil {
		if activeProvCounts["antigravity"] > 0 {
			for _, m := range cat.VerifiedAntigravityModels() {
				var qPct *int
				if m.QuotaRemaining != nil {
					v := int(*m.QuotaRemaining*100 + 0.5)
					qPct = &v
				}
				addModel("antigravity/"+m.ID, "antigravity", "oauth", m.MaxTokens, m.MaxOutputTokens, qPct, m.Capabilities)
			}
		}
		for _, m := range cat.VerifiedKiloFreeModels() {
			addModel("kilo-gateway/"+m.ID, "kilo-gateway", "free", m.MaxTokens, m.MaxOutputTokens, nil, m.Capabilities)
		}
	}

	// 3. Connected DeepSeek Web models
	if activeProvCounts["deepseek-web"] > 0 {
		for _, id := range []string{
			"deepseek-web/deepseek-chat",
			"deepseek-web/deepseek-reasoner",
			"deepseek-web/deepseek-v4-pro",
			"deepseek-web/deepseek-v4-flash",
		} {
			caps := providers.GetCapabilitiesForModel("deepseek-web", id)
			addModel(id, "deepseek-web", "cookie-free", 0, 0, nil, caps)
		}
	}

	// Sort models deterministically (unified pools first, then by provider + id)
	sort.Slice(models, func(i, j int) bool {
		ui := models[i].Provider == "9router-unified"
		uj := models[j].Provider == "9router-unified"
		if ui != uj {
			return ui
		}
		if models[i].Provider != models[j].Provider {
			return models[i].Provider < models[j].Provider
		}
		return models[i].ID < models[j].ID
	})
	if len(models) > 120 {
		models = models[:120]
	}

	// Sanitized provider status summary (built from usageSummary + active counts, never exposing account names/keys)
	agMinQuota := map[string]*int{}
	for _, pu := range usage.ByProvider {
		if pu.QuotaRemainingPct != nil {
			v := *pu.QuotaRemainingPct
			agMinQuota[pu.Provider] = &v
		}
	}
	var provSummaries []publicProviderSummary
	seenProv := make(map[string]bool)
	for prov, cnt := range activeProvCounts {
		seenProv[prov] = true
		_, pol := providerLimitPolicy(prov)
		st := "active"
		if prov == "antigravity" && strings.EqualFold(autoSnap.OverallHealth, "autonomous") {
			st = "self-managing"
		}
		provSummaries = append(provSummaries, publicProviderSummary{
			Provider:    prov,
			AuthType:    activeProvAuth[prov],
			Accounts:    cnt,
			Status:      st,
			ModelCount:  provModelCounts[prov],
			QuotaPct:    agMinQuota[prov],
			LimitPolicy: pol,
		})
	}
	if !seenProv["kilo-gateway"] {
		_, pol := providerLimitPolicy("kilo-gateway")
		provSummaries = append(provSummaries, publicProviderSummary{
			Provider:    "kilo-gateway",
			AuthType:    "no-auth",
			Accounts:    1,
			Status:      "ready",
			ModelCount:  provModelCounts["kilo-gateway"],
			LimitPolicy: pol,
		})
	}
	sort.Slice(provSummaries, func(i, j int) bool {
		if provSummaries[i].ModelCount != provSummaries[j].ModelCount {
			return provSummaries[i].ModelCount > provSummaries[j].ModelCount
		}
		return provSummaries[i].Provider < provSummaries[j].Provider
	})

	var dbConnected bool
	if h.Repo != nil && h.Repo.RawDB() != nil {
		dbConnected = h.Repo.RawDB().Ping() == nil
	}

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"brand":           "9Router Gateway",
		"version":         updater.CurrentVersion,
		"uptime":          time.Since(h.Runtime.StartTime).Round(time.Second).String(),
		"endpointRoot":    baseRoot,
		"endpointV1":      baseV1,
		"dashboardOnline": probeHTTPTarget(h.Runtime.DashboardURL),
		"dbConnected":     dbConnected,
		"autonomyHealth":  autoSnap.OverallHealth,
		"stats": map[string]any{
			"modelsCount":          len(seenModels),
			"activeProvidersCount": len(provSummaries),
			"verifiedCatalogCount": autoSnap.Catalog.VerifiedCount,
			"requests24h":          usage.Requests24h,
			"tokens24h":            usage.Tokens24h,
			"totalRequests":        usage.TotalRequests,
			"totalTokens":          usage.TotalTokens,
		},
		"providers":    provSummaries,
		"unifiedPools": unifiedPools,
		"models":       models,
	})
}

var _ = (*db.Repo)(nil)
