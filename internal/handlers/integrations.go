package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"9router/proxy/internal/db"
	"9router/proxy/internal/handlerutil"
)

type cliIntegrationItem struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Installed  bool   `json:"installed"`
	Linked     bool   `json:"linked"`
	CanApply   bool   `json:"canApply"`
	ConfigPath string `json:"configPath,omitempty"`
	CurrentURL string `json:"currentUrl,omitempty"`
	Snippet    string `json:"snippet"`
}

func resolveGatewayBaseURL(r *http.Request, listenAddr string) string {
	host := r.Host
	if host == "" {
		host = listenAddr
	}
	if strings.HasPrefix(host, "0.0.0.0:") {
		host = "127.0.0.1:" + strings.TrimPrefix(host, "0.0.0.0:")
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + host
}

func homeDirOrEmpty() string {
	h, _ := os.UserHomeDir()
	return h
}

func resolveHermesDir() string {
	if envDir := strings.TrimSpace(os.Getenv("HERMES_HOME")); envDir != "" {
		return envDir
	}
	home := homeDirOrEmpty()
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			winDir := filepath.Join(local, "hermes")
			if fi, err := os.Stat(winDir); err == nil && fi.IsDir() {
				return winDir
			}
			// Hermes v0.22+ defaults to %LOCALAPPDATA%\hermes on Windows
			if home == "" {
				return winDir
			}
			if _, err := os.Stat(filepath.Join(home, ".hermes")); err != nil {
				return winDir
			}
		}
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".hermes")
}

func resolveClaudeDir() string {
	if envDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); envDir != "" {
		return envDir
	}
	home := homeDirOrEmpty()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".claude")
}

func resolveOpenCodeConfigPath() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "opencode", "opencode.json")
	}
	home := homeDirOrEmpty()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".config", "opencode", "opencode.json")
}

func resolveCodexDir() string {
	if envDir := strings.TrimSpace(os.Getenv("CODEX_HOME")); envDir != "" {
		return envDir
	}
	home := homeDirOrEmpty()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".codex")
}

func firstActiveAPIKey(repo *db.Repo) (id, rawKey, preview string) {
	if repo == nil || repo.RawDB() == nil {
		return "", "", "<YOUR_9ROUTER_API_KEY>"
	}
	err := repo.RawDB().QueryRow(
		"SELECT id, key FROM apiKeys WHERE isActive = 1 AND key != '' ORDER BY createdAt ASC LIMIT 1",
	).Scan(&id, &rawKey)
	if err != nil || rawKey == "" {
		return "", "", "<YOUR_9ROUTER_API_KEY>"
	}
	p := rawKey
	if len(p) > 12 {
		p = p[:8] + "..." + p[len(p)-4:]
	}
	return id, rawKey, p
}

func fileContainsGateway(path, baseRoot string) (bool, string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, ""
	}
	gwBase := strings.ToLower(strings.TrimSpace(baseRoot))
	text := string(b)
	for _, line := range strings.Split(text, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") || strings.HasPrefix(trim, "//") {
			continue
		}
		l := strings.ToLower(trim)
		hasKey := strings.Contains(l, "base_url") || strings.Contains(l, "baseurl") || strings.Contains(l, "anthropic_base_url")
		if !hasKey {
			continue
		}
		if (gwBase != "" && strings.Contains(l, gwBase)) ||
			(strings.Contains(l, ":20128") && (strings.Contains(l, "127.0.0.1") || strings.Contains(l, "localhost") || strings.Contains(l, "0.0.0.0"))) {
			return true, trim
		}
	}
	return false, ""
}

func binInstalled(bin, configPath string) bool {
	if _, err := exec.LookPath(bin); err == nil {
		return true
	}
	if configPath != "" {
		if _, err := os.Stat(configPath); err == nil {
			return true
		}
	}
	return false
}

// HandleIntegrationsGet inspects installed CLI tools and returns live 1-gateway setup snippets.
// GET /api/gateway/integrations
func (h *GatewayPanelHandler) HandleIntegrationsGet(w http.ResponseWriter, r *http.Request) {
	baseRoot := resolveGatewayBaseURL(r, h.Runtime.ListenAddr)
	baseV1 := baseRoot + "/v1"
	_, _, keyPreview := firstActiveAPIKey(h.Repo)

	hermesDir := resolveHermesDir()
	hermesCfg := filepath.Join(hermesDir, "config.yaml")
	hermesLinked, hermesCur := fileContainsGateway(hermesCfg, baseRoot)

	claudeCfg := filepath.Join(resolveClaudeDir(), "settings.json")
	claudeLinked, claudeCur := fileContainsGateway(claudeCfg, baseRoot)

	ocCfg := resolveOpenCodeConfigPath()
	ocLinked, ocCur := fileContainsGateway(ocCfg, baseRoot)

	codexCfg := filepath.Join(resolveCodexDir(), "config.toml")
	codexLinked, codexCur := fileContainsGateway(codexCfg, baseRoot)

	items := []cliIntegrationItem{
		{
			ID:         "hermes",
			Name:       "Hermes Agent",
			Installed:  binInstalled("hermes", hermesCfg),
			Linked:     hermesLinked,
			CanApply:   true,
			ConfigPath: hermesCfg,
			CurrentURL: hermesCur,
			Snippet: fmt.Sprintf("# %s\nmodel:\n  default: \"auto\"\n  provider: \"custom\"\n  base_url: %q\n# .env -> OPENAI_API_KEY=%s",
				hermesCfg, baseV1, keyPreview),
		},
		{
			ID:         "claude",
			Name:       "Claude Code",
			Installed:  binInstalled("claude", claudeCfg),
			Linked:     claudeLinked,
			CanApply:   true,
			ConfigPath: claudeCfg,
			CurrentURL: claudeCur,
			Snippet: fmt.Sprintf("ANTHROPIC_BASE_URL=%s\nANTHROPIC_AUTH_TOKEN=%s\nANTHROPIC_MODEL=claude-sonnet-4-6",
				baseRoot, keyPreview),
		},
		{
			ID:         "opencode",
			Name:       "OpenCode",
			Installed:  binInstalled("opencode", ocCfg),
			Linked:     ocLinked,
			CanApply:   true,
			ConfigPath: ocCfg,
			CurrentURL: ocCur,
			Snippet: fmt.Sprintf("{\n  \"provider\": { \"9router\": { \"npm\": \"@ai-sdk/openai-compatible\", \"options\": { \"baseURL\": %q, \"apiKey\": %q } } },\n  \"model\": \"9router/auto\"\n}",
				baseV1, keyPreview),
		},
		{
			ID:         "codex",
			Name:       "Codex CLI",
			Installed:  binInstalled("codex", codexCfg),
			Linked:     codexLinked,
			CanApply:   true,
			ConfigPath: codexCfg,
			CurrentURL: codexCur,
			Snippet: fmt.Sprintf("model = \"auto\"\nmodel_provider = \"9router\"\n\n[model_providers.9router]\nname = \"9Router 1-Gateway\"\nbase_url = %q\nwire_api = \"responses\"",
				baseV1),
		},
		{
			ID:        "openai-sdk",
			Name:      "Cursor / Cline / Kilo / OpenAI SDK",
			Installed: true,
			Linked:    true,
			CanApply:  false,
			Snippet: fmt.Sprintf("Base URL: %s\nAPI Key:  %s\nModel:    auto  (or free-pool, deepseek-chat, claude-sonnet-4-6)",
				baseV1, keyPreview),
		},
	}

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"endpointRoot":  baseRoot,
		"endpointV1":    baseV1,
		"apiKeyPreview": keyPreview,
		"models": []string{
			"auto",
			"free-pool",
			"auto-reasoning",
			"auto-vision",
			"deepseek-chat",
			"deepseek-reasoner",
			"deepseek-uncensored",
			"claude-sonnet-4-6",
			"gemini-3-flash",
			"antigravity/gemini-3-flash",
			"deepseek-web/deepseek-chat",
			"kilo-gateway/kilo-auto/free",
		},
		"tools": items,
	})
}

// HandleIntegrationsApply safely backs up and configures Hermes, Claude Code, OpenCode, or Codex
// to route through this 1-Gateway endpoint.
// POST /api/gateway/integrations/apply
func (h *GatewayPanelHandler) HandleIntegrationsApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tool  string `json:"tool"`
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "auto"
	}
	_, rawKey, _ := firstActiveAPIKey(h.Repo)
	if rawKey == "" {
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "no active API key in SQLite — create an API key in Dashboard first")
		return
	}

	baseRoot := resolveGatewayBaseURL(r, h.Runtime.ListenAddr)
	baseV1 := baseRoot + "/v1"

	var targetPath string
	var err error
	switch strings.ToLower(strings.TrimSpace(req.Tool)) {
	case "hermes":
		targetPath, err = applyHermesConfig(resolveHermesDir(), baseV1, rawKey, model)
	case "claude":
		targetPath, err = applyClaudeConfig(resolveClaudeDir(), baseRoot, rawKey, model)
	case "opencode":
		targetPath, err = applyOpenCodeConfig(resolveOpenCodeConfigPath(), baseV1, rawKey, model)
	case "codex":
		targetPath, err = applyCodexConfig(resolveCodexDir(), baseV1, rawKey, model)
	default:
		handlerutil.WriteJSONError(w, http.StatusBadRequest, "unsupported tool (expected hermes, claude, opencode, or codex)")
		return
	}
	if err != nil {
		handlerutil.WriteJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	handlerutil.WriteJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"tool":       req.Tool,
		"model":      model,
		"configPath": targetPath,
		"endpointV1": baseV1,
	})
}

func backupIfNeeded(path string) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return
	}
	bak := path + ".9router.bak"
	if _, err := os.Stat(bak); err == nil {
		return
	}
	_ = os.WriteFile(bak, b, 0600)
}

func upsertEnvVar(envPath, key, val string) error {
	if err := os.MkdirAll(filepath.Dir(envPath), 0700); err != nil {
		return err
	}
	backupIfNeeded(envPath)
	raw, _ := os.ReadFile(envPath)
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	updated := false
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, key+"=") || strings.HasPrefix(t, "# "+key+"=") || strings.HasPrefix(t, "#"+key+"=") {
			lines[i] = fmt.Sprintf("%s=%s", key, val)
			updated = true
			break
		}
	}
	if !updated {
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, fmt.Sprintf("%s=%s", key, val), "")
	}
	return os.WriteFile(envPath, []byte(strings.Join(lines, "\n")), 0600)
}

func applyHermesConfig(dir, baseV1, apiKey, model string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("cannot resolve Hermes config directory")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	backupIfNeeded(cfgPath)

	raw, _ := os.ReadFile(cfgPath)
	content := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(content, "\n")

	inModel := false
	modelIndent := ""
	hasModelBlock := false
	setDefault, setProvider, setBase := false, false, false

	var out []string
	flushMissingModelKeys := func() {
		indent := modelIndent
		if indent == "" {
			indent = "  "
		}
		if !setDefault {
			out = append(out, fmt.Sprintf("%sdefault: %q", indent, model))
			setDefault = true
		}
		if !setProvider {
			out = append(out, fmt.Sprintf("%sprovider: \"custom\"", indent))
			setProvider = true
		}
		if !setBase {
			out = append(out, fmt.Sprintf("%sbase_url: %q", indent, baseV1))
			setBase = true
		}
	}

	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if !inModel {
			if trim == "model:" || strings.HasPrefix(trim, "model: ") {
				inModel = true
				hasModelBlock = true
			}
			out = append(out, line)
			continue
		}
		// Leaving top-level `model:` section when encountering a non-indented non-comment key
		if trim != "" && !strings.HasPrefix(trim, "#") && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
			flushMissingModelKeys()
			inModel = false
			out = append(out, line)
			continue
		}
		lineIndent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		if modelIndent == "" && lineIndent != "" && trim != "" && !strings.HasPrefix(trim, "#") {
			modelIndent = lineIndent
		}
		indent := modelIndent
		if indent == "" {
			indent = "  "
		}
		if trim == "" || strings.HasPrefix(trim, "#") || lineIndent != indent {
			out = append(out, line)
			continue
		}
		switch {
		case strings.HasPrefix(trim, "default:") || strings.HasPrefix(trim, "model:"):
			if !setDefault {
				out = append(out, fmt.Sprintf("%sdefault: %q", indent, model))
				setDefault = true
			}
			continue
		case strings.HasPrefix(trim, "provider:"):
			if !setProvider {
				out = append(out, fmt.Sprintf("%sprovider: \"custom\"", indent))
				setProvider = true
			}
			continue
		case strings.HasPrefix(trim, "base_url:"):
			if !setBase {
				out = append(out, fmt.Sprintf("%sbase_url: %q", indent, baseV1))
				setBase = true
			}
			continue
		default:
			out = append(out, line)
		}
	}
	if inModel {
		flushMissingModelKeys()
	} else if !hasModelBlock {
		out = append(out,
			"model:",
			fmt.Sprintf("  default: %q", model),
			`  provider: "custom"`,
			fmt.Sprintf("  base_url: %q", baseV1),
			"",
		)
	}

	if err := os.WriteFile(cfgPath, []byte(strings.Join(out, "\n")), 0600); err != nil {
		return "", err
	}
	if err := upsertEnvVar(filepath.Join(dir, ".env"), "OPENAI_API_KEY", apiKey); err != nil {
		return "", err
	}
	return cfgPath, nil
}

func applyClaudeConfig(dir, baseRoot, apiKey, model string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("cannot resolve Claude Code config directory")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	cfgPath := filepath.Join(dir, "settings.json")
	backupIfNeeded(cfgPath)

	root := map[string]any{}
	if raw, err := os.ReadFile(cfgPath); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		_ = json.Unmarshal(raw, &root)
	}
	envMap, _ := root["env"].(map[string]any)
	if envMap == nil {
		envMap = map[string]any{}
	}
	envMap["ANTHROPIC_BASE_URL"] = baseRoot
	envMap["ANTHROPIC_AUTH_TOKEN"] = apiKey
	if model != "" {
		envMap["ANTHROPIC_MODEL"] = model
	}
	root["env"] = envMap

	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", err
	}
	return cfgPath, os.WriteFile(cfgPath, append(b, '\n'), 0600)
}

func applyOpenCodeConfig(cfgPath, baseV1, apiKey, model string) (string, error) {
	if cfgPath == "" {
		return "", fmt.Errorf("cannot resolve OpenCode config path")
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0700); err != nil {
		return "", err
	}
	backupIfNeeded(cfgPath)

	root := map[string]any{}
	if raw, err := os.ReadFile(cfgPath); err == nil && len(strings.TrimSpace(string(raw))) > 0 {
		_ = json.Unmarshal(raw, &root)
	}
	provs, _ := root["provider"].(map[string]any)
	if provs == nil {
		provs = map[string]any{}
	}
	provs["9router"] = map[string]any{
		"npm":  "@ai-sdk/openai-compatible",
		"name": "9Router 1-Gateway",
		"options": map[string]any{
			"baseURL": baseV1,
			"apiKey":  apiKey,
		},
		"models": map[string]any{
			"auto":                        map[string]any{"name": "9Router Auto (Multi-Provider)"},
			"free-pool":                   map[string]any{"name": "9Router Free Pool"},
			"deepseek-chat":               map[string]any{"name": "DeepSeek Chat"},
			"claude-sonnet-4-6":           map[string]any{"name": "Claude Sonnet"},
			"antigravity/gemini-3-flash":  map[string]any{"name": "Gemini 3 Flash (AGY)"},
			"kilo-gateway/kilo-auto/free": map[string]any{"name": "Kilo Auto Free"},
		},
	}
	root["provider"] = provs
	if strings.Contains(model, "/") {
		root["model"] = "9router/" + model
	} else {
		root["model"] = "9router/" + model
	}

	b, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return "", err
	}
	return cfgPath, os.WriteFile(cfgPath, append(b, '\n'), 0600)
}

func applyCodexConfig(dir, baseV1, apiKey, model string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("cannot resolve Codex config directory")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	cfgPath := filepath.Join(dir, "config.toml")
	backupIfNeeded(cfgPath)

	raw, _ := os.ReadFile(cfgPath)
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	var kept []string
	skipSection := false
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "[") {
			skipSection = trim == "[model_providers.9router]"
		}
		if skipSection {
			continue
		}
		if strings.HasPrefix(trim, "model =") || strings.HasPrefix(trim, "model=") ||
			strings.HasPrefix(trim, "model_provider =") || strings.HasPrefix(trim, "model_provider=") {
			continue
		}
		kept = append(kept, line)
	}
	header := []string{
		fmt.Sprintf("model = %q", model),
		`model_provider = "9router"`,
		"",
	}
	block := []string{
		"[model_providers.9router]",
		`name = "9Router 1-Gateway"`,
		fmt.Sprintf("base_url = %q", baseV1),
		`wire_api = "responses"`,
		"",
	}
	out := append(header, kept...)
	out = append(out, block...)
	if err := os.WriteFile(cfgPath, []byte(strings.Join(out, "\n")), 0600); err != nil {
		return "", err
	}

	authPath := filepath.Join(dir, "auth.json")
	backupIfNeeded(authPath)
	authObj := map[string]any{}
	if ab, err := os.ReadFile(authPath); err == nil {
		_ = json.Unmarshal(ab, &authObj)
	}
	authObj["OPENAI_API_KEY"] = apiKey
	ab, _ := json.MarshalIndent(authObj, "", "  ")
	if err := os.WriteFile(authPath, append(ab, '\n'), 0600); err != nil {
		return "", err
	}
	return cfgPath, nil
}
