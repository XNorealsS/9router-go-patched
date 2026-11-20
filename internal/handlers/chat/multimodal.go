package chat

import (
	"9router/proxy/internal/models"
	"9router/proxy/internal/providers"
)

// modelsProviderCtx holds the resolved connection + config for a single-model forward.
type modelsProviderCtx struct {
	conn        *models.ProviderConnection
	connData    *ConnectionData
	providerCfg *providers.ProviderConfig
	apiKey      string
	modelInfo   *ModelInfo
}

// ---- Video endpoints (xAI Grok Imagine) ----
