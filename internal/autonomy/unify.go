package autonomy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"9router/proxy/internal/db"
	"9router/proxy/internal/providers"
)

const defaultKiloModelsURL = "https://api.kilo.ai/api/gateway/models"

// SyncKiloFree discovers all zero-cost (:free) models and real context/output
// limits from Kilo Gateway's public model catalog (no API key required).
func (cm *CatalogManager) SyncKiloFree(ctx context.Context, client *http.Client) (int, error) {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	endpoint := strings.TrimSpace(os.Getenv("KILO_GATEWAY_MODELS_URL"))
	if endpoint == "" {
		endpoint = defaultKiloModelsURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "9router-gateway")

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBodyBytes))
	if err != nil || resp.StatusCode != http.StatusOK {
		return 0, err
	}

	var parsed struct {
		Data []struct {
			ID                  string `json:"id"`
			Name                string `json:"name"`
			ContextLength       int    `json:"context_length"`
			MaxCompletionTokens int    `json:"max_completion_tokens"`
			Pricing             struct {
				Prompt string `json:"prompt"`
			} `json:"pricing"`
			Architecture *struct {
				InputModalities []string `json:"input_modalities"`
			} `json:"architecture"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, err
	}

	var freeModels []ModelCapabilityInfo
	nowStr := time.Now().UTC().Format(time.RFC3339)
	for _, m := range parsed.Data {
		if m.ID == "" {
			continue
		}
		lowerID := strings.ToLower(m.ID)
		if !strings.Contains(lowerID, ":free") && !strings.HasSuffix(lowerID, "/free") && !strings.HasSuffix(lowerID, "-free") && m.Pricing.Prompt != "0" {
			continue
		}
		caps := providers.Capabilities{
			Tools:     true,
			Reasoning: strings.Contains(m.ID, "reason") || strings.Contains(m.ID, "think") || strings.Contains(m.ID, "r1"),
			Vision:    strings.Contains(m.ID, "-vl") || strings.Contains(m.ID, "vision") || strings.Contains(m.ID, "omni"),
		}
		if m.Architecture != nil {
			for _, mod := range m.Architecture.InputModalities {
				switch strings.ToLower(mod) {
				case "image":
					caps.Vision = true
				case "audio":
					caps.AudioInput = true
				case "video":
					caps.VideoInput = true
				}
			}
		}
		ctxLen := m.ContextLength
		if ctxLen <= 0 {
			ctxLen = 131072
		}
		maxOut := m.MaxCompletionTokens
		if maxOut <= 0 {
			maxOut = 8192
		}
		freeModels = append(freeModels, ModelCapabilityInfo{
			ID:              m.ID,
			DisplayName:     m.Name,
			Provider:        "kilo-gateway",
			Status:          StatusVerified,
			MaxTokens:       ctxLen,
			MaxOutputTokens: maxOut,
			Capabilities:    caps,
			LastVerifiedAt:  nowStr,
		})
	}
	sort.Slice(freeModels, func(i, j int) bool { return freeModels[i].ID < freeModels[j].ID })

	cm.mu.Lock()
	cm.kiloFreeModels = freeModels
	cm.mu.Unlock()

	if len(freeModels) > 0 && cm.recorder != nil {
		cm.recorder.Record(Event{
			Category: CategoryCatalog,
			Provider: "kilo-gateway",
			Detected: "public no-auth :free model catalog sync",
			Action:   "synchronized " + itoa(len(freeModels)) + " zero-cost Kilo Gateway models",
			Reason:   "zero-config free capacity discovery",
			Outcome:  OutcomeOK,
		})
	}
	return len(freeModels), nil
}

// VerifiedKiloFreeModels returns synced zero-cost Kilo Gateway models (or built-in defaults if offline).
func (cm *CatalogManager) VerifiedKiloFreeModels() []ModelCapabilityInfo {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	if len(cm.kiloFreeModels) > 0 {
		out := make([]ModelCapabilityInfo, len(cm.kiloFreeModels))
		copy(out, cm.kiloFreeModels)
		return out
	}
	defaults := []string{
		"kilo-auto/free",
		"nvidia/nemotron-3-ultra-550b-a55b:free",
		"nvidia/nemotron-3.5-super-vl:free",
		"nvidia/nemotron-3.5-lightning:free",
		"inclusionai/ling-3.2-flash:free",
		"baidu/ernie-5.5-flash-preview:free",
		"recursive/flint-v1.0:free",
		"liquid/lfm-3-1.7b:free",
	}
	out := make([]ModelCapabilityInfo, 0, len(defaults))
	for _, id := range defaults {
		out = append(out, ModelCapabilityInfo{
			ID:              id,
			Provider:        "kilo-gateway",
			Status:          StatusVerified,
			MaxTokens:       131072,
			MaxOutputTokens: 8192,
			Capabilities:    providers.Capabilities{Tools: true, Vision: strings.Contains(id, "-vl")},
		})
	}
	return out
}

// ModelLimits returns the best known (contextWindow, maxOutputTokens) for a provider/model.
func (cm *CatalogManager) ModelLimits(provider, model string) (int, int) {
	if cm != nil {
		cm.mu.RLock()
		if provider == "antigravity" {
			if m, ok := cm.models[model]; ok && m.MaxTokens > 0 {
				out := m.MaxOutputTokens
				ctx := m.MaxTokens
				cm.mu.RUnlock()
				return ctx, out
			}
		} else if provider == "kilo-gateway" {
			for _, m := range cm.kiloFreeModels {
				if m.ID == model && m.MaxTokens > 0 {
					cm.mu.RUnlock()
					return m.MaxTokens, m.MaxOutputTokens
				}
			}
		}
		cm.mu.RUnlock()
	}
	return providers.GetModelTokenLimits(model)
}

type modelGroupRule struct {
	matchers []string
	targets  []string
}

var unifiedFamilyRules = []modelGroupRule{
	{
		matchers: []string{"deepseek-chat", "deepseek-v4", "deepseek-v4-flash", "deepseek-v3"},
		targets:  []string{"deepseek/deepseek-chat", "deepseek-web/deepseek-chat", "kilo-gateway/inclusionai/ling-3.2-flash:free", "opencode/deepseek-v4.1-flash-free"},
	},
	{
		matchers: []string{"deepseek-reasoner", "deepseek-r1", "deepseek-v4-pro", "deepseek-v4-pro-think"},
		targets:  []string{"deepseek/deepseek-reasoner", "deepseek-web/deepseek-reasoner", "kilo-gateway/nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free"},
	},
	{
		matchers: []string{"deepseek-uncensored", "r1-1776", "deepseek-r1-1776", "uncensored"},
		targets: []string{
			"perplexity/r1-1776",
			"openrouter/perplexity/r1-1776",
			"venice/venice-uncensored",
			"openrouter/cognitivecomputations/dolphin-mistral-24b-venice-edition:free",
			"venice/deepseek-r1-671b",
			"chutes/deepseek-ai/DeepSeek-R1",
			"nvidia/deepseek-ai/deepseek-r1",
			"ollama/huihui_ai/deepseek-r1-abliterated",
		},
	},
	{
		matchers: []string{"claude-sonnet-4-6", "claude-sonnet-4.6", "claude-sonnet-5-5", "claude-sonnet"},
		targets:  []string{"anthropic/claude-sonnet-4-6", "claude/claude-sonnet-4-6", "antigravity/claude-sonnet-5-5-medium", "kilo-gateway/kilo-auto/free"},
	},
	{
		matchers: []string{"claude-opus-4-6", "claude-opus-5-5", "claude-opus"},
		targets:  []string{"anthropic/claude-opus-4-6", "claude/claude-opus-4-6", "antigravity/claude-opus-5-5-medium", "kilo-gateway/kilo-auto/free"},
	},
	{
		matchers: []string{"gemini-3-flash", "gemini-3.5-flash", "gemini-3.8-flash", "gemini-2.5-flash"},
		targets:  []string{"gemini/gemini-2.5-flash", "antigravity/gemini-3-flash", "kilo-gateway/kilo-auto/free"},
	},
	{
		matchers: []string{"gemini-3.1-pro", "gemini-4-hydrogen", "gemini-2.5-pro"},
		targets:  []string{"gemini/gemini-2.5-pro", "antigravity/gemini-4-hydrogen-medium", "kilo-gateway/nvidia/nemotron-3-ultra-550b-a55b:free"},
	},
}

// ResolveUnifiedPool resolves a bare/canonical model or unified meta-pool ("auto", "auto-free",
// "auto-reasoning", "auto-vision") into an ordered multi-provider slice of only active/no-auth targets.
func ResolveUnifiedPool(repo *db.Repo, cat *CatalogManager, requested string) []string {
	m := strings.ToLower(strings.TrimSpace(requested))
	if m == "" || strings.Contains(m, "/") {
		// Also handle Kilo free model names specified without provider prefix (e.g. "kilo-auto/free" already has "/",
		// so strip check handles bare ":free" IDs below).
		if strings.HasSuffix(m, ":free") && !strings.HasPrefix(m, "kilo-gateway/") {
			return []string{"kilo-gateway/" + requested}
		}
		return nil
	}

	isProviderEnabled := func(prov string) bool {
		if cfg, ok := providers.KnownProviders[prov]; ok && cfg.NoAuth {
			return true
		}
		if repo == nil {
			return false
		}
		conns, err := repo.GetProviderConnections(prov, true)
		return err == nil && len(conns) > 0
	}

	filterActive := func(candidates []string) []string {
		var out []string
		seen := make(map[string]bool)
		for _, c := range candidates {
			p, _, ok := strings.Cut(c, "/")
			if !ok || seen[c] {
				continue
			}
			if isProviderEnabled(p) {
				seen[c] = true
				out = append(out, c)
			}
		}
		return out
	}

	// 1. Built-in intelligent meta-pools unifying all active & free accounts.
	switch m {
	case "auto", "auto-free", "free-pool":
		return filterActive([]string{
			"antigravity/gemini-3-flash",
			"deepseek-web/deepseek-chat",
			"kilo-gateway/kilo-auto/free",
			"kilo-gateway/nvidia/nemotron-3.5-super-vl:free",
			"kilo-gateway/inclusionai/ling-3.2-flash:free",
			"kilo-gateway/recursive/flint-v1.0:free",
			"antigravity/claude-sonnet-5-5-medium",
		})
	case "auto-reasoning", "auto-think":
		return filterActive([]string{
			"antigravity/claude-opus-5-5-medium",
			"antigravity/gemini-4-hydrogen-medium",
			"deepseek-web/deepseek-reasoner",
			"kilo-gateway/nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free",
			"kilo-gateway/kilo-auto/free",
		})
	case "auto-vision", "auto-multimodal":
		return filterActive([]string{
			"antigravity/gemini-3-flash",
			"antigravity/claude-sonnet-5-5-medium",
			"kilo-gateway/nvidia/nemotron-3.5-super-vl:free",
		})
	}

	// 2. Same-model unification across providers.
	for _, rule := range unifiedFamilyRules {
		for _, pat := range rule.matchers {
			if m == pat {
				if hits := filterActive(rule.targets); len(hits) > 0 {
					return hits
				}
			}
		}
	}

	// 3. Direct bare model name match in live Antigravity or Kilo Free catalogs.
	if cat != nil {
		if isProviderEnabled("antigravity") {
			for _, ag := range cat.VerifiedAntigravityModels() {
				if strings.EqualFold(ag.ID, requested) {
					return []string{"antigravity/" + ag.ID}
				}
			}
		}
		for _, kg := range cat.VerifiedKiloFreeModels() {
			if strings.EqualFold(kg.ID, requested) {
				return []string{"kilo-gateway/" + kg.ID}
			}
		}
	}
	return nil
}

// UnifiedVirtualModels returns canonical unified names exposed on /v1/models.
func UnifiedVirtualModels() []string {
	return []string{
		"auto",
		"auto-reasoning",
		"auto-vision",
		"deepseek-chat",
		"deepseek-reasoner",
		"claude-sonnet-4-6",
		"gemini-3-flash",
	}
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%10]
		n /= 10
	}
	return string(b[i:])
}
