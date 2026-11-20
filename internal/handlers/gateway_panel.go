package handlers

import (
	_ "embed"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

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
