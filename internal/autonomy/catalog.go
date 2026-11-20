package autonomy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"9router/proxy/internal/providers"
	"9router/proxy/internal/translator"
)

const (
	defaultAntigravityModelsURL = "https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels"
	defaultAntigravityUA        = "antigravity/hub/2.24.0 darwin/arm64"
	maxCatalogBodyBytes         = 4 << 20 // 4 MiB safety cap
)

// ModelStatus classifies upstream model verification confidence.
type ModelStatus string

const (
	StatusVerified   ModelStatus = "verified"
	StatusUncertain  ModelStatus = "uncertain"
	StatusDeprecated ModelStatus = "deprecated"
)

// AliasStatus captures runtime verification state for an Antigravity synonym mapping.
type AliasStatus struct {
	Alias  string      `json:"alias"`
	Target string      `json:"target"`
	Status ModelStatus `json:"status"`
}

// ModelCapabilityInfo merges live upstream metadata and inferred capabilities.
type ModelCapabilityInfo struct {
	ID                string                 `json:"id"`
	DisplayName       string                 `json:"displayName,omitempty"`
	Provider          string                 `json:"provider"`
	Status            ModelStatus            `json:"status"`
	MaxTokens         int                    `json:"maxTokens,omitempty"`
	MaxOutputTokens   int                    `json:"maxOutputTokens,omitempty"`
	MinThinkingBudget int                    `json:"minThinkingBudget,omitempty"`
	Recommended       bool                   `json:"recommended,omitempty"`
	ImageGeneration   bool                   `json:"imageGeneration,omitempty"`
	WebSearch         bool                   `json:"webSearch,omitempty"`
	QuotaRemaining    *float64               `json:"quotaRemaining,omitempty"`
	QuotaResetTime    string                 `json:"quotaResetTime,omitempty"`
	Capabilities      providers.Capabilities `json:"capabilities"`
	LastVerifiedAt    string                 `json:"lastVerifiedAt,omitempty"`
	Error             string                 `json:"error,omitempty"`
}

// CatalogDiff records changes observed across live catalog synchronizations.
type CatalogDiff struct {
	Timestamp          string   `json:"timestamp"`
	Added              []string `json:"added,omitempty"`
	Removed            []string `json:"removed,omitempty"`
	DeprecatedAliases  []string `json:"deprecatedAliases,omitempty"`
	VerifiedModelCount int      `json:"verifiedModelCount"`
}

// CatalogSnapshot summarizes the current Antigravity model intelligence state.
type CatalogSnapshot struct {
	Provider        string                `json:"provider"`
	Synced          bool                  `json:"synced"`
	LastSyncAt      string                `json:"lastSyncAt,omitempty"`
	LastError       string                `json:"lastError,omitempty"`
	DefaultModelID  string                `json:"defaultModelId,omitempty"`
	VerifiedCount   int                   `json:"verifiedCount"`
	UncertainCount  int                   `json:"uncertainCount"`
	DeprecatedCount int                   `json:"deprecatedCount"`
	Models          []ModelCapabilityInfo `json:"models"`
	Aliases         []AliasStatus         `json:"aliases"`
	LastDiff        *CatalogDiff          `json:"lastDiff,omitempty"`
}

// CatalogManager discovers, tracks, and validates live upstream Antigravity models.
type CatalogManager struct {
	mu              sync.RWMutex
	modelsURL       string
	synced          bool
	lastSync        time.Time
	lastError       string
	defaultModelID  string
	models          map[string]*ModelCapabilityInfo
	kiloFreeModels  []ModelCapabilityInfo
	runtimeStatus   map[string]ModelCapabilityInfo // runtime-only deprecations/uncertain marks
	aliases         []AliasStatus
	lastDiff        *CatalogDiff
	recorder        *Recorder
}

// NewCatalogManager initializes a CatalogManager seeded cleanly from known synonyms.
func NewCatalogManager(recorder *Recorder) *CatalogManager {
	url := strings.TrimSpace(os.Getenv("ANTIGRAVITY_FETCH_MODELS_URL"))
	if url == "" {
		url = defaultAntigravityModelsURL
	}
	cm := &CatalogManager{
		modelsURL:     url,
		models:        make(map[string]*ModelCapabilityInfo),
		runtimeStatus: make(map[string]ModelCapabilityInfo),
		recorder:      recorder,
	}
	cm.seedInitialAliasesLocked()
	return cm
}

func (cm *CatalogManager) seedInitialAliasesLocked() {
	seenTargets := make(map[string]bool)
	aliases := make([]AliasStatus, 0, len(translator.AntigravityModelSynonyms))
	for alias, target := range translator.AntigravityModelSynonyms {
		aliases = append(aliases, AliasStatus{
			Alias:  alias,
			Target: target,
			Status: StatusUncertain,
		})
		if !seenTargets[target] {
			seenTargets[target] = true
			caps := providers.GetCapabilitiesForModel("antigravity", target)
			ctxLen, maxOut := providers.GetModelTokenLimits(target)
			cm.models[target] = &ModelCapabilityInfo{
				ID:              target,
				Provider:        "antigravity",
				Status:          StatusUncertain,
				MaxTokens:       ctxLen,
				MaxOutputTokens: maxOut,
				Capabilities:    caps,
			}
		}
	}
	sort.Slice(aliases, func(i, j int) bool { return aliases[i].Alias < aliases[j].Alias })
	cm.aliases = aliases
}

type upstreamModelsResp struct {
	DefaultAgentModelID string                   `json:"defaultAgentModelId"`
	ImageGenModelIDs    []string                 `json:"imageGenerationModelIds"`
	WebSearchModelIDs   []string                 `json:"webSearchModelIds"`
	Models              map[string]upstreamModel `json:"models"`
}

type upstreamModel struct {
	DisplayName       string          `json:"displayName"`
	MaxTokens         int             `json:"maxTokens"`
	MaxOutputTokens   int             `json:"maxOutputTokens"`
	MinThinkingBudget int             `json:"minThinkingBudget"`
	Recommended       bool            `json:"recommended"`
	SupportedMime     map[string]bool `json:"supportedMimeTypes"`
	QuotaInfo         *struct {
		RemainingFraction *float64 `json:"remainingFraction"`
		ResetTime         string   `json:"resetTime"`
	} `json:"quotaInfo"`
}

// SyncAntigravity fetches live model metadata and validates synonyms against Cloud Code.
func (cm *CatalogManager) SyncAntigravity(ctx context.Context, client *http.Client, accessToken, projectID string) (*CatalogDiff, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("missing access token")
	}
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}

	payloadObj := map[string]any{}
	if projectID != "" {
		payloadObj["project"] = projectID
	}
	bodyBytes, _ := json.Marshal(payloadObj)

	cm.mu.RLock()
	endpoint := cm.modelsURL
	cm.mu.RUnlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	ua := os.Getenv("ANTIGRAVITY_USER_AGENT")
	if ua == "" {
		ua = defaultAntigravityUA
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)

	resp, err := client.Do(req)
	if err != nil {
		cm.recordSyncError(err.Error())
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBodyBytes))
	if err != nil {
		cm.recordSyncError(err.Error())
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := fmt.Sprintf("HTTP %d from fetchAvailableModels", resp.StatusCode)
		cm.recordSyncError(msg)
		return nil, fmt.Errorf("%s", msg)
	}

	var parsed upstreamModelsResp
	if err := json.Unmarshal(raw, &parsed); err != nil {
		cm.recordSyncError("invalid JSON catalog: " + err.Error())
		return nil, err
	}
	if len(parsed.Models) == 0 {
		msg := "empty model catalog returned by upstream"
		cm.recordSyncError(msg)
		return nil, fmt.Errorf("%s", msg)
	}

	imgGenSet := make(map[string]bool, len(parsed.ImageGenModelIDs))
	for _, id := range parsed.ImageGenModelIDs {
		imgGenSet[id] = true
	}
	webSearchSet := make(map[string]bool, len(parsed.WebSearchModelIDs))
	for _, id := range parsed.WebSearchModelIDs {
		webSearchSet[id] = true
	}

	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)

	cm.mu.Lock()
	defer cm.mu.Unlock()

	prevVerified := make(map[string]bool)
	for id, m := range cm.models {
		if m.Status == StatusVerified {
			prevVerified[id] = true
		}
	}

	nextModels := make(map[string]*ModelCapabilityInfo, len(parsed.Models))
	var added []string
	for id, um := range parsed.Models {
		caps := parseMimeCapabilities(id, um, imgGenSet[id], webSearchSet[id])
		status := StatusVerified
		var runtimeErr string
		if rt, ok := cm.runtimeStatus[id]; ok && rt.Status == StatusDeprecated {
			status = StatusDeprecated
			runtimeErr = rt.Error
		}
		var qRem *float64
		var qReset string
		if um.QuotaInfo != nil {
			qRem = um.QuotaInfo.RemainingFraction
			qReset = um.QuotaInfo.ResetTime
		}
		if cm.synced && !prevVerified[id] {
			added = append(added, id)
		}
		nextModels[id] = &ModelCapabilityInfo{
			ID:                id,
			DisplayName:       um.DisplayName,
			Provider:          "antigravity",
			Status:            status,
			MaxTokens:         um.MaxTokens,
			MaxOutputTokens:   um.MaxOutputTokens,
			MinThinkingBudget: um.MinThinkingBudget,
			Recommended:       um.Recommended,
			ImageGeneration:   imgGenSet[id] || caps.ImageOutput,
			WebSearch:         webSearchSet[id] || caps.Search,
			QuotaRemaining:    qRem,
			QuotaResetTime:    qReset,
			Capabilities:      caps,
			LastVerifiedAt:    nowStr,
			Error:             runtimeErr,
		}
	}

	var removed []string
	if cm.synced {
		for prevID := range prevVerified {
			if _, stillPresent := nextModels[prevID]; !stillPresent {
				removed = append(removed, prevID)
				nextModels[prevID] = &ModelCapabilityInfo{
					ID:       prevID,
					Provider: "antigravity",
					Status:   StatusDeprecated,
					Error:    "removed from upstream fetchAvailableModels catalog",
				}
			}
		}
	}

	// Validate all configured synonyms against live catalog.
	var depAliases []string
	nextAliases := make([]AliasStatus, 0, len(translator.AntigravityModelSynonyms))
	for alias, target := range translator.AntigravityModelSynonyms {
		st := StatusDeprecated
		if info, ok := nextModels[target]; ok && info.Status == StatusVerified {
			st = StatusVerified
		} else {
			depAliases = append(depAliases, alias+"->"+target)
		}
		nextAliases = append(nextAliases, AliasStatus{
			Alias:  alias,
			Target: target,
			Status: st,
		})
	}
	sort.Slice(nextAliases, func(i, j int) bool { return nextAliases[i].Alias < nextAliases[j].Alias })
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(depAliases)

	cm.models = nextModels
	cm.aliases = nextAliases
	cm.defaultModelID = parsed.DefaultAgentModelID
	cm.synced = true
	cm.lastSync = now
	cm.lastError = ""

	diff := &CatalogDiff{
		Timestamp:          nowStr,
		Added:              added,
		Removed:            removed,
		DeprecatedAliases:  depAliases,
		VerifiedModelCount: len(parsed.Models),
	}
	cm.lastDiff = diff

	if cm.recorder != nil {
		outcome := OutcomeOK
		needsAdmin := false
		if len(depAliases) > 0 || len(removed) > 0 {
			outcome = OutcomeDegraded
			if len(depAliases) > 0 {
				cm.recorder.Record(Event{
					Category:   CategoryDeprecation,
					Provider:   "antigravity",
					Detected:   fmt.Sprintf("%d alias target(s) missing in live catalog", len(depAliases)),
					Action:     "flagged stale aliases and updated catalog status",
					Reason:     strings.Join(depAliases, ", "),
					Outcome:    OutcomeDegraded,
					NeedsAdmin: false,
				})
			}
		}
		cm.recorder.Record(Event{
			Category:   CategoryCatalog,
			Provider:   "antigravity",
			Detected:   "Antigravity live model catalog sync",
			Action:     fmt.Sprintf("synced %d upstream models (+%d / -%d)", len(parsed.Models), len(added), len(removed)),
			Reason:     "periodic/initial live capability & quota verification",
			Outcome:    outcome,
			NeedsAdmin: needsAdmin,
		})
	}
	return diff, nil
}

func (cm *CatalogManager) recordSyncError(msg string) {
	cm.mu.Lock()
	cm.lastError = msg
	cm.mu.Unlock()
}

func parseMimeCapabilities(id string, um upstreamModel, isImageGen, isWebSearch bool) providers.Capabilities {
	caps := providers.Capabilities{
		Tools:       true,
		ImageOutput: isImageGen || translator.IsAntigravityImageModel(id),
		Search:      isWebSearch,
		Reasoning:   um.MinThinkingBudget > 0 || strings.Contains(id, "thinking") || strings.Contains(id, "hydrogen") || strings.Contains(id, "sonnet") || strings.Contains(id, "opus"),
	}
	for mime := range um.SupportedMime {
		m := strings.ToLower(mime)
		switch {
		case strings.HasPrefix(m, "image/"):
			caps.Vision = true
		case m == "application/pdf":
			caps.PDF = true
		case strings.HasPrefix(m, "audio/"):
			caps.AudioInput = true
		case strings.HasPrefix(m, "video/"):
			caps.VideoInput = true
		}
	}
	// Preserve fallback known heuristics if mime map was omitted upstream.
	if !caps.Vision && (strings.Contains(id, "gemini") || strings.Contains(id, "claude")) && !isImageGen {
		caps.Vision = true
	}
	if !caps.PDF && (strings.Contains(id, "gemini") || strings.Contains(id, "claude")) {
		caps.PDF = true
	}
	return caps
}

// MarkModelDeprecated marks a model deprecated after an upstream 404 or deprecation notice.
func (cm *CatalogManager) MarkModelDeprecated(provider, model, reason string) {
	if provider != "antigravity" || model == "" {
		return
	}
	cm.mu.Lock()
	nowStr := time.Now().UTC().Format(time.RFC3339)
	entry := ModelCapabilityInfo{
		ID:             model,
		Provider:       "antigravity",
		Status:         StatusDeprecated,
		LastVerifiedAt: nowStr,
		Error:          reason,
	}
	cm.runtimeStatus[model] = entry
	if m, ok := cm.models[model]; ok {
		m.Status = StatusDeprecated
		m.Error = reason
	} else {
		cm.models[model] = &entry
	}
	cm.mu.Unlock()

	if cm.recorder != nil {
		cm.recorder.Record(Event{
			Category: CategoryDeprecation,
			Provider: provider,
			Model:    model,
			Detected: "upstream reported deprecated or missing model",
			Action:   "marked model deprecated in runtime catalog",
			Reason:   reason,
			Outcome:  OutcomeDegraded,
		})
	}
}

// MarkModelVerified records live execution success for a concrete model.
func (cm *CatalogManager) MarkModelVerified(provider, model string) {
	if provider != "antigravity" || model == "" {
		return
	}
	cm.mu.Lock()
	defer cm.mu.Unlock()
	delete(cm.runtimeStatus, model)
	nowStr := time.Now().UTC().Format(time.RFC3339)
	if m, ok := cm.models[model]; ok {
		m.Status = StatusVerified
		m.Error = ""
		m.LastVerifiedAt = nowStr
	} else {
		caps := providers.GetCapabilitiesForModel("antigravity", model)
		ctxLen, maxOut := providers.GetModelTokenLimits(model)
		cm.models[model] = &ModelCapabilityInfo{
			ID:              model,
			Provider:        "antigravity",
			Status:          StatusVerified,
			MaxTokens:       ctxLen,
			MaxOutputTokens: maxOut,
			Capabilities:    caps,
			LastVerifiedAt:  nowStr,
		}
	}
}

// ResolveHealthyModel normalizes an Antigravity model and substitutes a verified healthy
// fallback when the target is known deprecated in the live catalog.
func (cm *CatalogManager) ResolveHealthyModel(requested string) (target string, fallbackApplied bool, reason string) {
	normalized := translator.NormalizeAntigravityModel(requested)

	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if info, ok := cm.models[normalized]; ok && info.Status == StatusDeprecated {
		// Choose best verified alternative preserving Claude vs Gemini family where possible.
		isClaude := strings.Contains(strings.ToLower(normalized), "claude") || strings.Contains(strings.ToLower(requested), "claude")
		candidates := make([]*ModelCapabilityInfo, 0, len(cm.models))
		for _, m := range cm.models {
			if m.Status != StatusVerified || m.ImageGeneration {
				continue
			}
			mClaude := strings.Contains(strings.ToLower(m.ID), "claude")
			if isClaude == mClaude {
				candidates = append(candidates, m)
			}
		}
		if len(candidates) > 0 {
			sort.Slice(candidates, func(i, j int) bool {
				if candidates[i].Recommended != candidates[j].Recommended {
					return candidates[i].Recommended
				}
				return candidates[i].ID < candidates[j].ID
			})
			alt := candidates[0].ID
			return alt, true, fmt.Sprintf("%s is deprecated (%s); auto-routed to verified %s", normalized, info.Error, alt)
		}
		if cm.defaultModelID != "" {
			if def, ok := cm.models[cm.defaultModelID]; ok && def.Status == StatusVerified {
				return def.ID, true, fmt.Sprintf("%s is deprecated; auto-routed to upstream default %s", normalized, def.ID)
			}
		}
	}
	return normalized, false, ""
}

// QuotaRemaining returns the upstream quota fraction (0..1) if known, or -1 if unknown.
func (cm *CatalogManager) QuotaRemaining(model string) float64 {
	normalized := translator.NormalizeAntigravityModel(model)
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	if m, ok := cm.models[normalized]; ok && m.QuotaRemaining != nil {
		return *m.QuotaRemaining
	}
	return -1
}

// NeedsSync reports whether the Antigravity catalog has never synced or is older than ttl.
func (cm *CatalogManager) NeedsSync(ttl time.Duration) bool {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	if !cm.synced {
		return true
	}
	return time.Since(cm.lastSync) >= ttl
}

// CapabilitiesFor returns live-discovered capabilities if synced, else static defaults.
func (cm *CatalogManager) CapabilitiesFor(provider, model string) providers.Capabilities {
	if provider == "antigravity" {
		normalized := translator.NormalizeAntigravityModel(model)
		cm.mu.RLock()
		if m, ok := cm.models[normalized]; ok {
			cm.mu.RUnlock()
			return m.Capabilities
		}
		cm.mu.RUnlock()
	}
	return providers.GetCapabilitiesForModel(provider, model)
}

// VerifiedAntigravityModels returns all verified Antigravity models for /v1/models discovery.
func (cm *CatalogManager) VerifiedAntigravityModels() []ModelCapabilityInfo {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if !cm.synced {
		return nil
	}
	out := make([]ModelCapabilityInfo, 0, len(cm.models))
	for _, m := range cm.models {
		if m.Status == StatusVerified {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Snapshot returns an immutable summary for monitoring UI & diagnostics APIs.
func (cm *CatalogManager) Snapshot() CatalogSnapshot {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	models := make([]ModelCapabilityInfo, 0, len(cm.models)+len(cm.kiloFreeModels))
	var vCount, uCount, dCount int
	for _, m := range cm.models {
		models = append(models, *m)
		switch m.Status {
		case StatusVerified:
			vCount++
		case StatusDeprecated:
			dCount++
		default:
			uCount++
		}
	}
	for _, km := range cm.kiloFreeModels {
		models = append(models, km)
		vCount++
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].Status != models[j].Status {
			return models[i].Status == StatusVerified || (models[i].Status == StatusUncertain && models[j].Status == StatusDeprecated)
		}
		if models[i].Provider != models[j].Provider {
			return models[i].Provider < models[j].Provider
		}
		return models[i].ID < models[j].ID
	})

	aliases := make([]AliasStatus, len(cm.aliases))
	copy(aliases, cm.aliases)

	var lastSyncStr string
	if !cm.lastSync.IsZero() {
		lastSyncStr = cm.lastSync.Format(time.RFC3339)
	}
	var diffCopy *CatalogDiff
	if cm.lastDiff != nil {
		c := *cm.lastDiff
		diffCopy = &c
	}

	return CatalogSnapshot{
		Provider:        "antigravity",
		Synced:          cm.synced,
		LastSyncAt:      lastSyncStr,
		LastError:       cm.lastError,
		DefaultModelID:  cm.defaultModelID,
		VerifiedCount:   vCount,
		UncertainCount:  uCount,
		DeprecatedCount: dCount,
		Models:          models,
		Aliases:         aliases,
		LastDiff:        diffCopy,
	}
}
