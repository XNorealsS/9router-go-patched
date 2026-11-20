package autonomy

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"9router/proxy/internal/db"
	"9router/proxy/internal/log"
	"9router/proxy/internal/providers"
)

const (
	defaultTickInterval   = 2 * time.Minute
	defaultCatalogSyncTTL = 15 * time.Minute
)

// OAuthRefreshFunc refreshes an active connection's OAuth token and returns updated (accessToken, projectID, err).
type OAuthRefreshFunc func(ctx context.Context, connID, currentToken string) (string, string, error)

// ProjectDiscoverFunc discovers and persists an Antigravity projectId for connID.
type ProjectDiscoverFunc func(ctx context.Context, connID, accessToken string) (string, error)

// ConnectionHealth represents sanitized Antigravity connection automation status.
type ConnectionHealth struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ProjectID      string `json:"projectId,omitempty"`
	Active         bool   `json:"active"`
	TokenStatus    string `json:"tokenStatus"` // healthy | expiring | expired | missing
	ExpiresAt      string `json:"expiresAt,omitempty"`
	ExpiresInSecs  int64  `json:"expiresInSecs,omitempty"`
	BackoffLevel   int    `json:"backoffLevel"`
	NeedsAdmin     bool   `json:"needsAdmin"`
	AdminReason    string `json:"adminReason,omitempty"`
}

// SystemStatusSnapshot aggregates automation health for /api/gateway/system & UI.
type SystemStatusSnapshot struct {
	OverallHealth     string             `json:"overallHealth"` // autonomous | degraded | needs_admin | unconfigured
	Syncing           bool               `json:"syncing"`
	LastSyncAt        string             `json:"lastSyncAt,omitempty"`
	Connections       []ConnectionHealth `json:"connections"`
	Catalog           CatalogSnapshot    `json:"catalog"`
	RecentEvents      []Event            `json:"recentEvents"`
	PendingAdmin      []Event            `json:"pendingAdmin"`
	ActiveSessions    []SessionInfo      `json:"activeSessions"`
}

// Engine coordinates autonomous OAuth health, project discovery, model catalog sync,
// recovery event logging, and coding-agent session tracking.
type Engine struct {
	Repo       *db.Repo
	Client     *http.Client
	Catalog    *CatalogManager
	Recorder   *Recorder
	RefreshFn  OAuthRefreshFunc
	DiscoverFn ProjectDiscoverFunc
	CatalogTTL time.Duration

	mu          sync.RWMutex
	lastSyncAt  time.Time
	syncRunning atomic.Bool

	adminFlagsMu sync.RWMutex
	adminFlags   map[string]string // connID -> reason
}

var (
	activeCatalogMu sync.RWMutex
	activeCatalog   *CatalogManager
)

// NewEngine constructs an autonomous gateway engine and registers its catalog for routing checks.
func NewEngine(repo *db.Repo, client *http.Client) *Engine {
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	rec := NewRecorder()
	cat := NewCatalogManager(rec)
	activeCatalogMu.Lock()
	activeCatalog = cat
	activeCatalogMu.Unlock()
	return &Engine{
		Repo:       repo,
		Client:     client,
		Recorder:   rec,
		Catalog:    cat,
		CatalogTTL: defaultCatalogSyncTTL,
		adminFlags: make(map[string]string),
	}
}

// LookupCapabilities returns live-synced Antigravity capabilities when available, else static defaults.
func LookupCapabilities(provider, model string) providers.Capabilities {
	if provider == "antigravity" {
		activeCatalogMu.RLock()
		cat := activeCatalog
		activeCatalogMu.RUnlock()
		if cat != nil {
			return cat.CapabilitiesFor(provider, model)
		}
	}
	return providers.GetCapabilitiesForModel(provider, model)
}

// FlagConnectionNeedsAdmin marks a connection as requiring operator intervention.
func (e *Engine) FlagConnectionNeedsAdmin(connID, reason string) {
	if e == nil || connID == "" {
		return
	}
	e.adminFlagsMu.Lock()
	e.adminFlags[connID] = reason
	e.adminFlagsMu.Unlock()
}

// ClearConnectionAdminFlag clears any prior admin intervention warning after successful recovery.
func (e *Engine) ClearConnectionAdminFlag(connID string) {
	if e == nil || connID == "" {
		return
	}
	e.adminFlagsMu.Lock()
	delete(e.adminFlags, connID)
	e.adminFlagsMu.Unlock()
}

// Start runs the lightweight periodic automation loop until ctx is cancelled.
func (e *Engine) Start(ctx context.Context) {
	if e == nil {
		return
	}
	go func() {
		// Short initial delay so HTTP server binds immediately without blocking startup.
		select {
		case <-ctx.Done():
			return
		case <-time.After(1200 * time.Millisecond):
		}
		e.SyncNow(ctx, "startup")

		ticker := time.NewTicker(defaultTickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.SyncNow(ctx, "periodic")
			}
		}
	}()
}

// SyncNow executes one non-overlapping Antigravity OAuth, project, and catalog synchronization pass.
func (e *Engine) SyncNow(ctx context.Context, trigger string) {
	if e == nil || e.Repo == nil {
		return
	}
	if !e.syncRunning.CompareAndSwap(false, true) {
		return
	}
	defer e.syncRunning.Store(false)

	conns, err := e.Repo.GetProviderConnections("antigravity", true)
	if err != nil {
		log.Warn("autonomy", "list antigravity connections failed", "error", err)
		return
	}
	if len(conns) == 0 {
		e.mu.Lock()
		e.lastSyncAt = time.Now().UTC()
		e.mu.Unlock()
		return
	}

	catalogSyncedThisPass := false
	forceCatalog := trigger == "manual" || trigger == "startup"

	for _, c := range conns {
		if ctx.Err() != nil {
			return
		}
		var dataMap map[string]any
		if err := json.Unmarshal([]byte(c.Data), &dataMap); err != nil {
			e.FlagConnectionNeedsAdmin(c.ID, "corrupt connection JSON metadata")
			e.Recorder.Record(Event{
				Category:     CategoryOAuth,
				Provider:     "antigravity",
				ConnectionID: c.ID,
				Detected:     "corrupt connection JSON in database",
				Action:       "skipped connection",
				Reason:       err.Error(),
				Outcome:      OutcomeNeedsAdmin,
				NeedsAdmin:   true,
			})
			continue
		}

		oauthData := providers.ParseOAuthConnection(dataMap)
		if oauthData == nil {
			continue
		}
		token := oauthData.AccessToken
		projectID := oauthData.ProjectID

		// 1. Proactive OAuth token refresh before expiry.
		if (oauthData.IsExpired() || token == "") && oauthData.RefreshToken != "" && e.RefreshFn != nil {
			refTok, refPID, rErr := e.RefreshFn(ctx, c.ID, token)
			if rErr != nil {
				plan := PlanRecovery("antigravity", "", 401, rErr.Error(), 0)
				if plan.NeedsAdmin {
					e.FlagConnectionNeedsAdmin(c.ID, plan.Reason)
				}
				e.Recorder.Record(Event{
					Category:     CategoryOAuth,
					Provider:     "antigravity",
					ConnectionID: c.ID,
					Detected:     "proactive OAuth refresh required (" + trigger + ")",
					Action:       "attempted token refresh",
					Reason:       rErr.Error(),
					Outcome:      outcomeForNeedsAdmin(plan.NeedsAdmin),
					NeedsAdmin:   plan.NeedsAdmin,
				})
			} else {
				if refTok != "" {
					token = refTok
				}
				if refPID != "" {
					projectID = refPID
				}
				e.ClearConnectionAdminFlag(c.ID)
				e.Recorder.Record(Event{
					Category:     CategoryOAuth,
					Provider:     "antigravity",
					ConnectionID: c.ID,
					Detected:     "OAuth access token expired or within expiry buffer",
					Action:       "proactively refreshed token and persisted credentials",
					Reason:       "maintain uninterrupted Antigravity sessions (" + trigger + ")",
					Outcome:      OutcomeOK,
				})
			}
		}

		// 2. Automatic Cloud Code project discovery when missing.
		if projectID == "" && token != "" && e.DiscoverFn != nil {
			pid, pErr := e.DiscoverFn(ctx, c.ID, token)
			if pErr != nil {
				e.FlagConnectionNeedsAdmin(c.ID, pErr.Error())
				e.Recorder.Record(Event{
					Category:     CategoryProject,
					Provider:     "antigravity",
					ConnectionID: c.ID,
					Detected:     "Antigravity connection missing projectId",
					Action:       "attempted loadCodeAssist/onboardUser discovery",
					Reason:       pErr.Error(),
					Outcome:      OutcomeNeedsAdmin,
					NeedsAdmin:   true,
				})
			} else if pid != "" {
				projectID = pid
				e.ClearConnectionAdminFlag(c.ID)
				e.Recorder.Record(Event{
					Category:     CategoryProject,
					Provider:     "antigravity",
					ConnectionID: c.ID,
					Detected:     "Antigravity connection missing projectId",
					Action:       "discovered and persisted Cloud Code projectId=" + pid,
					Reason:       "required by v1internal Antigravity endpoints",
					Outcome:      OutcomeOK,
				})
			}
		}

		// 3. Synchronize live model catalog once per pass if stale or forced.
		if !catalogSyncedThisPass && token != "" && (forceCatalog || e.Catalog.NeedsSync(e.CatalogTTL)) {
			if _, cErr := e.Catalog.SyncAntigravity(ctx, e.Client, token, projectID); cErr != nil {
				e.Recorder.Record(Event{
					Category:     CategoryCatalog,
					Provider:     "antigravity",
					ConnectionID: c.ID,
					Detected:     "live catalog sync failed",
					Action:       "kept previous/static model definitions",
					Reason:       cErr.Error(),
					Outcome:      OutcomeDegraded,
				})
			} else {
				catalogSyncedThisPass = true
			}
		}
	}

	e.mu.Lock()
	e.lastSyncAt = time.Now().UTC()
	e.mu.Unlock()
}

func outcomeForNeedsAdmin(needsAdmin bool) Outcome {
	if needsAdmin {
		return OutcomeNeedsAdmin
	}
	return OutcomeFailed
}

// StatusSnapshot returns the unified automation state for observability and the /gateway console.
func (e *Engine) StatusSnapshot() SystemStatusSnapshot {
	if e == nil {
		return SystemStatusSnapshot{OverallHealth: "unconfigured"}
	}
	cat := e.Catalog.Snapshot()
	events := e.Recorder.RecentEvents(60, "")
	pending := e.Recorder.PendingAdminEvents()
	sessions := e.Recorder.ActiveSessions(20)

	var connHealth []ConnectionHealth
	hasAdmin := len(pending) > 0
	hasActive := false

	if e.Repo != nil {
		if rows, err := e.Repo.GetProviderConnections("antigravity", false); err == nil {
			now := time.Now().UTC()
			e.adminFlagsMu.RLock()
			for _, c := range rows {
				ch := ConnectionHealth{
					ID:     c.ID,
					Active: c.IsActive == 1,
				}
				if c.Name != nil {
					ch.Name = *c.Name
				}
				if reason, ok := e.adminFlags[c.ID]; ok {
					ch.NeedsAdmin = true
					ch.AdminReason = reason
					hasAdmin = true
				}
				var dataMap map[string]any
				if json.Unmarshal([]byte(c.Data), &dataMap) == nil {
					oauthData := providers.ParseOAuthConnection(dataMap)
					if oauthData != nil {
						ch.ProjectID = oauthData.ProjectID
						ch.ExpiresAt = oauthData.ExpiresAt
						switch {
						case oauthData.AccessToken == "":
							ch.TokenStatus = "missing"
						case oauthData.IsExpired():
							ch.TokenStatus = "expired"
						default:
							ch.TokenStatus = "healthy"
							if exp, err := time.Parse(time.RFC3339, oauthData.ExpiresAt); err == nil {
								rem := int64(exp.Sub(now).Seconds())
								ch.ExpiresInSecs = rem
								if rem < 300 {
									ch.TokenStatus = "expiring"
								}
							}
						}
					}
					if lvl, ok := dataMap["backoffLevel"].(float64); ok {
						ch.BackoffLevel = int(lvl)
					}
				}
				if ch.Active {
					hasActive = true
				}
				connHealth = append(connHealth, ch)
			}
			e.adminFlagsMu.RUnlock()
		}
	}

	overall := "autonomous"
	switch {
	case !hasActive:
		overall = "unconfigured"
	case hasAdmin:
		overall = "needs_admin"
	case cat.LastError != "" || cat.DeprecatedCount > 0:
		overall = "degraded"
	}

	e.mu.RLock()
	lastSync := e.lastSyncAt
	e.mu.RUnlock()
	var lastSyncStr string
	if !lastSync.IsZero() {
		lastSyncStr = lastSync.Format(time.RFC3339)
	}

	return SystemStatusSnapshot{
		OverallHealth:  overall,
		Syncing:        e.syncRunning.Load(),
		LastSyncAt:     lastSyncStr,
		Connections:    connHealth,
		Catalog:        cat,
		RecentEvents:   events,
		PendingAdmin:   pending,
		ActiveSessions: sessions,
	}
}
