package executor

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/bits"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"9router/proxy/internal/proxy"
	"9router/proxy/internal/shutdown"
	"9router/proxy/internal/translator"
)

const (
	defaultDeepSeekWebBase = "https://chat.deepseek.com"
	maxDeepSeekPoWDiff     = 250000
	maxDeepSeekRespBytes   = 4 << 20
	deepSeekTokenTTL       = 55 * time.Minute
	maxDeepSeekTokenCache  = 100
)

type dsTokenEntry struct {
	token     string
	expiresAt time.Time
}

var (
	dsTokenMu    sync.Mutex
	dsTokenCache = make(map[string]dsTokenEntry)
	dsStreamSeq  atomic.Uint64
)

// extractDeepSeekCreds accepts any credential shape saved by Next.js Web Login or manual cookie paste:
//   - raw userToken string
//   - localStorage JSON {"value":"<token>"}
//   - Cookie string containing userToken=... / __Secure-userToken=... plus session cookies
func extractDeepSeekCreds(raw string) (bearerToken, cookieHeader string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ""
	}
	// JSON shape from browser localStorage: {"value":"...", ...}
	if strings.HasPrefix(trimmed, "{") {
		var obj struct {
			Value string `json:"value"`
			Token string `json:"token"`
		}
		if json.Unmarshal([]byte(trimmed), &obj) == nil {
			if obj.Value != "" {
				return obj.Value, ""
			}
			if obj.Token != "" {
				return obj.Token, ""
			}
		}
	}
	// Cookie header shape: "key1=val1; userToken=...; ds_session=..."
	if strings.Contains(trimmed, "=") && strings.Contains(trimmed, ";") {
		cookieHeader = trimmed
		for _, part := range strings.Split(trimmed, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok {
				continue
			}
			kl := strings.ToLower(strings.TrimSpace(k))
			val := strings.Trim(strings.TrimSpace(v), `"`)
			if val == "" {
				continue
			}
			if kl == "usertoken" || kl == "user_token" || kl == "__secure-usertoken" {
				// URL-encoded JSON localStorage value sometimes pasted inside cookie jars
				if unesc, err := url.QueryUnescape(val); err == nil && strings.HasPrefix(unesc, "{") {
					if tok, _ := extractDeepSeekCreds(unesc); tok != "" {
						bearerToken = tok
						break
					}
				}
				bearerToken = val
				break
			}
		}
		return bearerToken, cookieHeader
	}
	return trimmed, ""
}

func syntheticDeepSeekCookie() string {
	now := time.Now().UnixMilli()
	var buf [9]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("intercom-HWWAFSESTIME=%d; HWWAFSESID=%s; Hm_lvt=%d",
		now, hex.EncodeToString(buf[:]), now/1000)
}

func deepSeekWebOrigin(cfgBaseURL string) string {
	u, err := url.Parse(strings.TrimSpace(cfgBaseURL))
	if err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return defaultDeepSeekWebBase
}

func applyDeepSeekHeaders(req *http.Request, origin, bearerToken, extraCookie string) {
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36")
	req.Header.Set("X-Client-Bundle-Id", "com.deepseek.chat")
	req.Header.Set("X-Client-Locale", "en-US")
	req.Header.Set("X-Client-Platform", "web")
	req.Header.Set("X-Client-Version", "2.0.0")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	if extraCookie != "" {
		req.Header.Set("Cookie", extraCookie)
	} else {
		req.Header.Set("Cookie", syntheticDeepSeekCookie())
	}
}

func dsCacheKey(origin, creds string) string {
	sum := sha256.Sum256([]byte(origin + "|" + creds))
	return hex.EncodeToString(sum[:])
}

// acquireDeepSeekAccessToken exchanges a pasted userToken or browser cookie via /api/v0/users/current.
func acquireDeepSeekAccessToken(ctx context.Context, client *http.Client, origin, userToken, extraCookie string) (string, error) {
	cKey := dsCacheKey(origin, userToken+"|"+extraCookie)
	now := time.Now()
	dsTokenMu.Lock()
	if ent, ok := dsTokenCache[cKey]; ok && now.Before(ent.expiresAt) {
		dsTokenMu.Unlock()
		return ent.token, nil
	}
	dsTokenMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/v0/users/current", nil)
	if err != nil {
		return "", err
	}
	applyDeepSeekHeaders(req, origin, userToken, extraCookie)

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", &proxy.UpstreamError{StatusCode: resp.StatusCode, Body: body}
	}
	if resp.StatusCode != http.StatusOK {
		return "", &proxy.UpstreamError{StatusCode: resp.StatusCode, Body: body}
	}

	var parsed struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data *struct {
			BizCode int    `json:"biz_code"`
			BizMsg  string `json:"biz_msg"`
			BizData *struct {
				Token string `json:"token"`
			} `json:"biz_data"`
		} `json:"data"`
		BizData *struct {
			Token string `json:"token"`
		} `json:"biz_data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse users/current response: %w", err)
	}
	if parsed.Code != 0 {
		return "", &proxy.UpstreamError{
			StatusCode: http.StatusUnauthorized,
			Body:       []byte(fmt.Sprintf(`{"error":{"message":"DeepSeek rejected cookie/userToken (code %d: %s)"}}`, parsed.Code, parsed.Msg)),
		}
	}
	tok := ""
	if parsed.Data != nil && parsed.Data.BizData != nil && parsed.Data.BizData.Token != "" {
		tok = parsed.Data.BizData.Token
	} else if parsed.BizData != nil && parsed.BizData.Token != "" {
		tok = parsed.BizData.Token
	}
	if tok == "" {
		// Cookie-only sessions sometimes authenticate directly via Cookie header without returning a rotated token.
		if userToken != "" {
			tok = userToken
		} else if extraCookie != "" {
			return "", nil
		} else {
			return "", &proxy.UpstreamError{StatusCode: http.StatusUnauthorized, Body: body}
		}
	}

	dsTokenMu.Lock()
	if len(dsTokenCache) >= maxDeepSeekTokenCache {
		for k := range dsTokenCache {
			delete(dsTokenCache, k)
			break
		}
	}
	dsTokenCache[cKey] = dsTokenEntry{token: tok, expiresAt: now.Add(deepSeekTokenTTL)}
	dsTokenMu.Unlock()
	return tok, nil
}

type dsPowChallenge struct {
	Algorithm  string `json:"algorithm"`
	Challenge  string `json:"challenge"`
	Salt       string `json:"salt"`
	Difficulty int    `json:"difficulty"`
	ExpireAt   int64  `json:"expire_at"`
	Signature  string `json:"signature"`
	TargetPath string `json:"target_path"`
}

func fetchDeepSeekPoWChallenge(ctx context.Context, client *http.Client, origin, accessToken, extraCookie, targetPath string) (*dsPowChallenge, error) {
	payload, _ := json.Marshal(map[string]string{"target_path": targetPath})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/api/v0/chat/create_pow_challenge", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	applyDeepSeekHeaders(req, origin, accessToken, extraCookie)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &proxy.UpstreamError{StatusCode: resp.StatusCode, Body: body}
	}

	var parsed struct {
		Code int `json:"code"`
		Data *struct {
			BizData *struct {
				Challenge *dsPowChallenge `json:"challenge"`
			} `json:"biz_data"`
		} `json:"data"`
		BizData *struct {
			Challenge *dsPowChallenge `json:"challenge"`
		} `json:"biz_data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse pow challenge: %w", err)
	}
	var ch *dsPowChallenge
	if parsed.Data != nil && parsed.Data.BizData != nil && parsed.Data.BizData.Challenge != nil {
		ch = parsed.Data.BizData.Challenge
	} else if parsed.BizData != nil {
		ch = parsed.BizData.Challenge
	}
	if ch == nil || ch.Challenge == "" || ch.Difficulty < 1 || ch.Difficulty > maxDeepSeekPoWDiff {
		return nil, fmt.Errorf("invalid DeepSeek PoW challenge response")
	}
	if ch.Algorithm != "DeepSeekHashV1" {
		return nil, fmt.Errorf("unsupported DeepSeek PoW algorithm: %s", ch.Algorithm)
	}
	ch.TargetPath = targetPath
	return ch, nil
}

// --- Native 23-round Keccak-f[1600] solver matching DeepSeekHashV1 (round 0 skipped) ---

var (
	dsKeccakPI   = [24]int{10, 7, 11, 17, 18, 3, 5, 16, 8, 21, 24, 4, 15, 23, 19, 13, 12, 2, 20, 14, 22, 9, 6, 1}
	dsKeccakROTL = [24]int{1, 3, 6, 10, 15, 21, 28, 36, 45, 55, 2, 14, 27, 41, 56, 8, 25, 43, 62, 18, 39, 61, 20, 44}
	dsKeccakRC   = [24]uint64{
		0x0000000000000001, 0x0000000000008082, 0x800000000000808a, 0x8000000080008000,
		0x000000000000808b, 0x0000000080000001, 0x8000000080008081, 0x8000000000008009,
		0x000000000000008a, 0x0000000000000088, 0x0000000080008009, 0x000000008000000a,
		0x000000008000808b, 0x800000000000008b, 0x8000000000008089, 0x8000000000008003,
		0x8000000000008002, 0x8000000000000080, 0x000000000000800a, 0x800000008000000a,
		0x8000000080008081, 0x8000000000008080, 0x0000000080000001, 0x8000000080008008,
	}
)

func keccakP23(s *[25]uint64) {
	var bc [5]uint64
	// DeepSeekHashV1 runs 23 rounds starting at round index 1 (skipping round 0).
	for round := 1; round < 24; round++ {
		for x := 0; x < 5; x++ {
			bc[x] = s[x] ^ s[x+5] ^ s[x+10] ^ s[x+15] ^ s[x+20]
		}
		for x := 0; x < 5; x++ {
			t := bc[(x+4)%5] ^ bits.RotateLeft64(bc[(x+1)%5], 1)
			for y := 0; y < 25; y += 5 {
				s[x+y] ^= t
			}
		}
		cur := s[1]
		for t := 0; t < 24; t++ {
			idx := dsKeccakPI[t]
			cur, s[idx] = s[idx], bits.RotateLeft64(cur, dsKeccakROTL[t])
		}
		for y := 0; y < 25; y += 5 {
			a0, a1, a2, a3, a4 := s[y], s[y+1], s[y+2], s[y+3], s[y+4]
			s[y] = a0 ^ ((^a1) & a2)
			s[y+1] = a1 ^ ((^a2) & a3)
			s[y+2] = a2 ^ ((^a3) & a4)
			s[y+3] = a3 ^ ((^a4) & a0)
			s[y+4] = a4 ^ ((^a0) & a1)
		}
		s[0] ^= dsKeccakRC[round]
	}
}

func deepSeekHashV1(input []byte) [32]byte {
	const block = 136
	var st [25]uint64
	var buf [block]byte
	pos := 0
	for _, b := range input {
		buf[pos] = b
		pos++
		if pos == block {
			for i := 0; i < block/8; i++ {
				st[i] ^= binary.LittleEndian.Uint64(buf[i*8 : i*8+8])
			}
			keccakP23(&st)
			pos = 0
			buf = [block]byte{}
		}
	}
	buf[pos] ^= 0x06
	buf[block-1] ^= 0x80
	for i := 0; i < block/8; i++ {
		st[i] ^= binary.LittleEndian.Uint64(buf[i*8 : i*8+8])
	}
	keccakP23(&st)

	var out [32]byte
	for i := 0; i < 4; i++ {
		binary.LittleEndian.PutUint64(out[i*8:i*8+8], st[i])
	}
	return out
}

func solveDeepSeekPoW(ch *dsPowChallenge) (int, error) {
	targetBytes, err := hex.DecodeString(strings.ToLower(strings.TrimSpace(ch.Challenge)))
	if err != nil || len(targetBytes) != 32 {
		return -1, fmt.Errorf("invalid DeepSeek PoW challenge hex")
	}
	var target [32]byte
	copy(target[:], targetBytes)

	prefix := ch.Salt + "_" + strconv.FormatInt(ch.ExpireAt, 10) + "_"
	msg := make([]byte, len(prefix), len(prefix)+12)
	copy(msg, prefix)
	for nonce := 0; nonce < ch.Difficulty; nonce++ {
		cand := strconv.AppendInt(msg[:len(prefix)], int64(nonce), 10)
		if deepSeekHashV1(cand) == target {
			return nonce, nil
		}
	}
	return -1, fmt.Errorf("DeepSeek PoW nonce not found within difficulty=%d", ch.Difficulty)
}

func createDeepSeekSession(ctx context.Context, client *http.Client, origin, accessToken, extraCookie string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/api/v0/chat_session/create", strings.NewReader(`{}`))
	if err != nil {
		return "", err
	}
	applyDeepSeekHeaders(req, origin, accessToken, extraCookie)
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", &proxy.UpstreamError{StatusCode: resp.StatusCode, Body: body}
	}

	var parsed struct {
		Code int `json:"code"`
		Data *struct {
			BizData *struct {
				ChatSession *struct {
					ID string `json:"id"`
				} `json:"chat_session"`
			} `json:"biz_data"`
		} `json:"data"`
		BizData *struct {
			ChatSession *struct {
				ID string `json:"id"`
			} `json:"chat_session"`
		} `json:"biz_data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("parse chat_session/create: %w", err)
	}
	if parsed.Data != nil && parsed.Data.BizData != nil && parsed.Data.BizData.ChatSession != nil && parsed.Data.BizData.ChatSession.ID != "" {
		return parsed.Data.BizData.ChatSession.ID, nil
	}
	if parsed.BizData != nil && parsed.BizData.ChatSession != nil && parsed.BizData.ChatSession.ID != "" {
		return parsed.BizData.ChatSession.ID, nil
	}
	return "", fmt.Errorf("missing chat_session.id in DeepSeek create session response")
}

func deleteDeepSeekSession(origin, accessToken, extraCookie, sessionID string) {
	if sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]string{"chat_session_id": sessionID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/api/v0/chat_session/delete", bytes.NewReader(payload))
	if err != nil {
		return
	}
	applyDeepSeekHeaders(req, origin, accessToken, extraCookie)
	req.Header.Set("Content-Type", "application/json")
	if resp, err := (&http.Client{Timeout: 4 * time.Second}).Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

func buildDeepSeekPrompt(reqBody map[string]any) (prompt, model string, modelType string, thinking, search bool) {
	model, _ = reqBody["model"].(string)
	if model == "" {
		model = "deepseek-chat"
	}
	lower := strings.ToLower(model)
	modelType = "default"
	if strings.Contains(lower, "pro") || strings.Contains(lower, "expert") {
		modelType = "expert"
	}
	thinking = strings.Contains(lower, "r1") || strings.Contains(lower, "think") || strings.Contains(lower, "reason")
	if v, ok := reqBody["thinking_enabled"].(bool); ok && v {
		thinking = true
	}
	if v, ok := reqBody["reasoning_effort"].(string); ok && v != "" && v != "none" {
		thinking = true
	}
	search = strings.Contains(lower, "search")
	if v, ok := reqBody["search_enabled"].(bool); ok && v {
		search = true
	}

	msgs, _ := reqBody["messages"].([]any)
	var sysParts []string
	var convo []string
	toolNames := make(map[string]string)
	for _, rawM := range msgs {
		m, ok := rawM.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		text := extractDeepSeekMsgText(m["content"])
		switch role {
		case "system", "developer":
			if text != "" {
				sysParts = append(sysParts, text)
			}
		case "user":
			if text != "" {
				convo = append(convo, "User: "+text)
			}
		case "assistant":
			if calls, ok := m["tool_calls"].([]any); ok {
				for _, tc := range calls {
					if tcm, ok := tc.(map[string]any); ok {
						id, _ := tcm["id"].(string)
						if fn, ok := tcm["function"].(map[string]any); ok {
							name, _ := fn["name"].(string)
							if id != "" && name != "" {
								toolNames[id] = name
							}
						}
					}
				}
			}
			if text != "" {
				convo = append(convo, "Assistant: "+text)
			}
		case "tool":
			id, _ := m["tool_call_id"].(string)
			name := toolNames[id]
			if name == "" {
				name, _ = m["name"].(string)
			}
			if name == "" {
				name = "tool"
			}
			if text != "" {
				convo = append(convo, fmt.Sprintf("Tool result (%s): %s", name, text))
			}
		}
	}

	var sections []string
	if len(sysParts) > 0 {
		sections = append(sections, strings.Join(sysParts, "\n\n"))
	}
	if len(convo) == 1 && strings.HasPrefix(convo[0], "User: ") {
		sections = append(sections, strings.TrimPrefix(convo[0], "User: "))
	} else if len(convo) > 0 {
		if len(convo) > 20 {
			convo = convo[len(convo)-20:]
		}
		sections = append(sections, strings.Join(convo, "\n\n"))
	}
	prompt = strings.Join(sections, "\n\n")
	return
}

func extractDeepSeekMsgText(c any) string {
	switch v := c.(type) {
	case string:
		return strings.TrimSpace(v)
	case []any:
		var parts []string
		for _, item := range v {
			if pm, ok := item.(map[string]any); ok && pm["type"] == "text" {
				if t, ok := pm["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}

type dsCite struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Index int    `json:"cite_index"`
}

var citeTagRe = regexp.MustCompile(`\[citation:(\d+)\]`)

// ForwardDeepSeekWeb executes chat/completion against DeepSeek Web using pasted userToken or browser cookies,
// handling /users/current token acquisition, native Go 23-round Keccak PoW challenge, session creation,
// SSE patch parsing (thinking/content/citations), and optional Anthropic Messages translation.
func ForwardDeepSeekWeb(w http.ResponseWriter, req *Request) error {
	userToken, extraCookie := extractDeepSeekCreds(req.APIKey)
	if userToken == "" && extraCookie == "" {
		return &proxy.UpstreamError{
			StatusCode: http.StatusUnauthorized,
			Body:       []byte(`{"error":{"message":"Missing DeepSeek userToken or cookies"}}`),
		}
	}
	client := req.Client
	if client == nil {
		client = http.DefaultClient
	}
	origin := defaultDeepSeekWebBase
	if req.Config != nil && req.Config.BaseURL != "" {
		origin = deepSeekWebOrigin(req.Config.BaseURL)
	}

	var parsedReq map[string]any
	if err := json.Unmarshal(req.Body, &parsedReq); err != nil {
		return fmt.Errorf("parse request body: %w", err)
	}
	prompt, modelName, modelType, thinking, search := buildDeepSeekPrompt(parsedReq)

	accessToken, err := acquireDeepSeekAccessToken(req.Ctx, client, origin, userToken, extraCookie)
	if err != nil {
		return fmt.Errorf("deepseek-web auth: %w", err)
	}

	powCh, err := fetchDeepSeekPoWChallenge(req.Ctx, client, origin, accessToken, extraCookie, "/api/v0/chat/completion")
	if err != nil {
		return fmt.Errorf("deepseek-web pow challenge: %w", err)
	}
	answer, err := solveDeepSeekPoW(powCh)
	if err != nil {
		return fmt.Errorf("deepseek-web pow solve: %w", err)
	}
	powRespBytes, _ := json.Marshal(map[string]any{
		"algorithm":   powCh.Algorithm,
		"challenge":   powCh.Challenge,
		"salt":        powCh.Salt,
		"answer":      answer,
		"signature":   powCh.Signature,
		"target_path": powCh.TargetPath,
	})
	powHeader := base64.StdEncoding.EncodeToString(powRespBytes)

	sessionID, err := createDeepSeekSession(req.Ctx, client, origin, accessToken, extraCookie)
	if err != nil {
		return fmt.Errorf("deepseek-web session: %w", err)
	}
	defer deleteDeepSeekSession(origin, accessToken, extraCookie, sessionID)

	_, tzOffset := time.Now().Zone()
	payload, _ := json.Marshal(map[string]any{
		"chat_session_id":   sessionID,
		"parent_message_id": nil,
		"model_type":        modelType,
		"prompt":            prompt,
		"ref_file_ids":      []any{},
		"thinking_enabled":  thinking,
		"search_enabled":    search,
		"preempt":           false,
	})

	compURL := origin + "/api/v0/chat/completion"
	httpReq, err := http.NewRequestWithContext(req.Ctx, http.MethodPost, compURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	applyDeepSeekHeaders(httpReq, origin, accessToken, extraCookie)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Ds-Pow-Response", powHeader)
	httpReq.Header.Set("X-Client-Timezone-Offset", strconv.Itoa(tzOffset))

	resp, err := client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("deepseek-web completion: %w", err)
	}
	stallReader := proxy.NewStallReader(resp.Body, 0, "deepseek-web")
	defer stallReader.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(stallReader, 1<<20))
		return &proxy.UpstreamError{StatusCode: resp.StatusCode, Body: errBody}
	}

	if req.IsStream {
		return streamDeepSeekWebSSE(w, stallReader, req, modelName, search)
	}
	return collectDeepSeekWebNonStream(req.Ctx, w, stallReader, modelName, search, req.TranslateResp, req.ResponseBuf)
}

func emitOpenAIChunk(w http.ResponseWriter, flusher http.Flusher, req *Request, trSessionKey, chunkID, model string, created int64, delta map[string]any, finishReason *string) {
	payload := map[string]any{
		"id":      chunkID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	sseLine := append([]byte("data: "), raw...)
	sseLine = append(sseLine, '\n', '\n')

	if req.TranslateResp {
		if out, err := translator.TranslateOpenAIToClaudeStreamSession(trSessionKey, raw); err == nil && len(out) > 0 {
			if req.ResponseBuf != nil {
				_, _ = req.ResponseBuf.Write(out)
			}
			_, _ = w.Write(out)
			if flusher != nil {
				flusher.Flush()
			}
		}
		return
	}
	if req.TTFT != nil && *req.TTFT == 0 && !req.StartTime.IsZero() {
		*req.TTFT = time.Since(req.StartTime).Milliseconds()
	}
	if req.ResponseBuf != nil {
		_, _ = req.ResponseBuf.Write(sseLine)
	}
	_, _ = w.Write(sseLine)
	if flusher != nil {
		flusher.Flush()
	}
}

func streamDeepSeekWebSSE(w http.ResponseWriter, upstream io.Reader, req *Request, modelName string, search bool) error {
	flusher := proxy.WriteSSEHeaders(w)
	seq := dsStreamSeq.Add(1)
	chunkID := fmt.Sprintf("chatcmpl-dsw-%d", seq)
	created := time.Now().Unix()
	trSessionKey := fmt.Sprintf("dsw-tr-%d", seq)
	if req.TranslateResp {
		defer translator.ClearStreamState(trSessionKey)
	}

	sentRole := false
	phase := ""
	var cites []dsCite
	finished := false
	silentSearch := strings.Contains(strings.ToLower(modelName), "search-silent")
	isThinkModel := strings.Contains(strings.ToLower(modelName), "think") || strings.Contains(strings.ToLower(modelName), "r1") || strings.Contains(strings.ToLower(modelName), "reason")

	sendText := func(text string) {
		text = cleanDeepSeekFragment(text, search, silentSearch)
		if text == "" {
			return
		}
		if !sentRole {
			sentRole = true
			emitOpenAIChunk(w, flusher, req, trSessionKey, chunkID, modelName, created, map[string]any{"role": "assistant", "content": ""}, nil)
		}
		curPhase := phase
		if curPhase == "" && isThinkModel {
			curPhase = "thinking"
		}
		if curPhase == "thinking" {
			emitOpenAIChunk(w, flusher, req, trSessionKey, chunkID, modelName, created, map[string]any{"reasoning_content": text}, nil)
		} else {
			emitOpenAIChunk(w, flusher, req, trSessionKey, chunkID, modelName, created, map[string]any{"content": text}, nil)
		}
	}

	scanErr := proxy.ScanStream(upstream, func(raw []byte) {
		s := strings.TrimSpace(string(raw))
		if s == "[DONE]" {
			finished = true
			return
		}
		var evt struct {
			P string `json:"p"`
			O string `json:"o"`
			V any    `json:"v"`
		}
		if json.Unmarshal([]byte(s), &evt) != nil {
			return
		}
		handleDSWEvent(evt.P, evt.O, evt.V, &phase, &cites, &finished, sendText)
	})
	if shutdown.Fired() || !finished {
		// Emit cleanly even if upstream closed right after FINISHED patch
	}
	if refs := formatDeepSeekCites(cites, silentSearch); refs != "" {
		sendText("\n\n" + refs)
	}
	if !sentRole {
		emitOpenAIChunk(w, flusher, req, trSessionKey, chunkID, modelName, created, map[string]any{"role": "assistant", "content": ""}, nil)
	}
	stop := "stop"
	emitOpenAIChunk(w, flusher, req, trSessionKey, chunkID, modelName, created, map[string]any{}, &stop)
	if !req.TranslateResp {
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	return scanErr
}

func collectDeepSeekWebNonStream(ctx context.Context, w http.ResponseWriter, upstream io.Reader, modelName string, search, translate bool, respBuf io.Writer) error {
	var contentBuf, reasonBuf strings.Builder
	phase := ""
	var cites []dsCite
	finished := false
	silentSearch := strings.Contains(strings.ToLower(modelName), "search-silent")
	isThinkModel := strings.Contains(strings.ToLower(modelName), "think") || strings.Contains(strings.ToLower(modelName), "r1") || strings.Contains(strings.ToLower(modelName), "reason")

	sendText := func(text string) {
		text = cleanDeepSeekFragment(text, search, silentSearch)
		if text == "" {
			return
		}
		curPhase := phase
		if curPhase == "" && isThinkModel {
			curPhase = "thinking"
		}
		if curPhase == "thinking" {
			reasonBuf.WriteString(text)
		} else {
			contentBuf.WriteString(text)
		}
	}

	if err := proxy.ScanStream(upstream, func(raw []byte) {
		s := strings.TrimSpace(string(raw))
		if s == "[DONE]" {
			finished = true
			return
		}
		var evt struct {
			P string `json:"p"`
			O string `json:"o"`
			V any    `json:"v"`
		}
		if json.Unmarshal([]byte(s), &evt) != nil {
			return
		}
		handleDSWEvent(evt.P, evt.O, evt.V, &phase, &cites, &finished, sendText)
	}); err != nil {
		return err
	}
	if refs := formatDeepSeekCites(cites, silentSearch); refs != "" {
		contentBuf.WriteString("\n\n" + refs)
	}

	msg := map[string]any{
		"role":    "assistant",
		"content": contentBuf.String(),
	}
	if reasonBuf.Len() > 0 {
		msg["reasoning_content"] = reasonBuf.String()
	}
	outBytes, _ := json.Marshal(map[string]any{
		"id":      fmt.Sprintf("chatcmpl-dsw-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   modelName,
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": "stop",
		}},
		"usage": map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	})
	return jsonResponse(ctx, w, bytes.NewReader(outBytes), translate, respBuf)
}

func handleDSWEvent(path, op string, val any, phase *string, cites *[]dsCite, finished *bool, emit func(string)) {
	if path == "response/status" {
		if s, ok := val.(string); ok && s == "FINISHED" {
			*finished = true
		}
		return
	}
	if path == "response/search_status" {
		return
	}
	if path == "response/search_results" {
		if arr, ok := val.([]any); ok {
			if op != "BATCH" {
				*cites = (*cites)[:0]
				for _, item := range arr {
					if m, ok := item.(map[string]any); ok {
						c := dsCite{}
						c.URL, _ = m["url"].(string)
						c.Title, _ = m["title"].(string)
						if idx, ok := m["cite_index"].(float64); ok {
							c.Index = int(idx)
						}
						*cites = append(*cites, c)
					}
				}
			}
		}
		return
	}
	if vm, ok := val.(map[string]any); ok {
		if respObj, ok := vm["response"].(map[string]any); ok {
			if te, ok := respObj["thinking_enabled"].(bool); ok {
				if te {
					*phase = "thinking"
				} else {
					*phase = "content"
				}
			}
			if frags, ok := respObj["fragments"].([]any); ok {
				for _, f := range frags {
					handleFragmentMap(f, phase, emit, false)
				}
			}
		}
	}
	if path == "response/fragments" {
		if arr, ok := val.([]any); ok {
			for _, f := range arr {
				handleFragmentMap(f, phase, emit, true)
			}
		} else {
			handleFragmentMap(val, phase, emit, true)
		}
		return
	}
	if s, ok := val.(string); ok {
		emit(s)
		return
	}
	if path == "response" {
		if arr, ok := val.([]any); ok {
			for _, item := range arr {
				if m, ok := item.(map[string]any); ok {
					if mp, _ := m["p"].(string); mp == "response" {
						if mv, ok := m["v"].(map[string]any); ok {
							if te, ok := mv["thinking_enabled"].(bool); ok && te {
								*phase = "thinking"
							}
						}
					}
					if mv, ok := m["v"].([]any); ok {
						for _, f := range mv {
							handleFragmentMap(f, phase, emit, true)
						}
					}
				}
			}
		}
	}
}

func handleFragmentMap(v any, phase *string, emit func(string), setPhase bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	tp := strings.ToUpper(fmt.Sprintf("%v", m["type"]))
	if tp == "THINK" {
		*phase = "thinking"
	} else if tp == "ANSWER" || tp == "RESPONSE" {
		*phase = "content"
	} else if !setPhase && tp != "" && tp != "<NIL>" {
		*phase = "content"
	}
	if c, ok := m["content"].(string); ok && c != "" {
		emit(c)
	}
}

func cleanDeepSeekFragment(s string, search, silent bool) string {
	s = strings.ReplaceAll(s, "FINISHED", "")
	if search {
		if silent {
			s = citeTagRe.ReplaceAllString(s, "")
		} else {
			s = citeTagRe.ReplaceAllString(s, "[$1]")
		}
	}
	return s
}

func formatDeepSeekCites(cites []dsCite, silent bool) string {
	if len(cites) == 0 || silent {
		return ""
	}
	var valid []dsCite
	for _, c := range cites {
		if c.Index > 0 && c.URL != "" {
			valid = append(valid, c)
		}
	}
	if len(valid) == 0 {
		return ""
	}
	sort.Slice(valid, func(i, j int) bool { return valid[i].Index < valid[j].Index })
	var lines []string
	for _, c := range valid {
		lines = append(lines, fmt.Sprintf("[%d]: [%s](%s)", c.Index, c.Title, c.URL))
	}
	return strings.Join(lines, "\n")
}
