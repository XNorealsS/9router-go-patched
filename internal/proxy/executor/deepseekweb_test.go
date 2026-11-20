package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"9router/proxy/internal/providers"
)

func TestExtractDeepSeekCreds_SupportsJSONRawAndCookies(t *testing.T) {
	tok, ck := extractDeepSeekCreds(`{"value":"tok-json-123","__version":"0"}`)
	if tok != "tok-json-123" || ck != "" {
		t.Fatalf("unexpected JSON creds parse: tok=%q ck=%q", tok, ck)
	}

	tok, ck = extractDeepSeekCreds("ds_session_id=abc; userToken=tok-cookie-456; HWWAFSESID=xyz")
	if tok != "tok-cookie-456" || !strings.Contains(ck, "HWWAFSESID=xyz") {
		t.Fatalf("unexpected Cookie creds parse: tok=%q ck=%q", tok, ck)
	}

	tok, ck = extractDeepSeekCreds("raw-bearer-789")
	if tok != "raw-bearer-789" || ck != "" {
		t.Fatalf("unexpected raw creds parse: tok=%q ck=%q", tok, ck)
	}
}

func TestDeepSeekHashV1_KnownVectorAndPoWSolve(t *testing.T) {
	// Cross-verified against open-sse/lib/deepseek-pow-hash.js on "salt_1700000000_7".
	const wantHex = "5013d0ba33b12e18f572058fd0c9bf8d0b8e1d036f9e03c340672f511630b23a"
	digest := deepSeekHashV1([]byte("salt_1700000000_7"))
	if gotHex := hex.EncodeToString(digest[:]); gotHex != wantHex {
		t.Fatalf("23-round Keccak mismatch vs JS pow worker:\ngot:  %s\nwant: %s", gotHex, wantHex)
	}
	ch := &dsPowChallenge{
		Algorithm:  "DeepSeekHashV1",
		Challenge:  wantHex,
		Salt:       "salt",
		ExpireAt:   1700000000,
		Difficulty: 20,
	}
	nonce, err := solveDeepSeekPoW(ch)
	if err != nil || nonce != 7 {
		t.Fatalf("expected nonce=7, got nonce=%d err=%v", nonce, err)
	}
}

func TestForwardDeepSeekWeb_EndToEndMockCookieFlow(t *testing.T) {
	expectedPrefix := "mysalt_1700000000_"
	expectedNonce := 3
	digest := deepSeekHashV1([]byte(expectedPrefix + "3"))
	challengeHex := hex.EncodeToString(digest[:])

	var sawCookie, sawPow bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Cookie"), "ds_sess=1") {
			sawCookie = true
		}
		switch r.URL.Path {
		case "/api/v0/users/current":
			if r.Header.Get("Authorization") != "Bearer tok-from-cookie" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"biz_data": map[string]any{"token": "exchanged-jwt"}},
			})
		case "/api/v0/chat/create_pow_challenge":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{
					"biz_data": map[string]any{
						"challenge": map[string]any{
							"algorithm":  "DeepSeekHashV1",
							"challenge":  challengeHex,
							"salt":       "mysalt",
							"difficulty": 10,
							"expire_at":  1700000000,
							"signature":  "sig-1",
						},
					},
				},
			})
		case "/api/v0/chat_session/create":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0,
				"data": map[string]any{"biz_data": map[string]any{"chat_session": map[string]any{"id": "sess-dsw-1"}}},
			})
		case "/api/v0/chat/completion":
			if r.Header.Get("X-Ds-Pow-Response") != "" {
				sawPow = true
			}
			_ = expectedNonce
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"p\":\"response/fragments\",\"v\":[{\"type\":\"THINK\",\"content\":\"step1\"}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"p\":\"response/fragments\",\"v\":[{\"type\":\"RESPONSE\",\"content\":\"DEEPSEEK_WEB_OK\"}]}\n\n"))
			_, _ = w.Write([]byte("data: {\"p\":\"response/status\",\"v\":\"FINISHED\"}\n\n"))
		case "/api/v0/chat_session/delete":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// Clear cache before running
	dsTokenMu.Lock()
	dsTokenCache = make(map[string]dsTokenEntry)
	dsTokenMu.Unlock()
	_ = sha256.Size

	rec := httptest.NewRecorder()
	err := ForwardDeepSeekWeb(rec, &Request{
		Ctx:    t.Context(),
		Client: srv.Client(),
		Config: &providers.ProviderConfig{BaseURL: srv.URL + "/api/v0/chat/completion"},
		APIKey: "ds_sess=1; userToken=tok-from-cookie",
		Body:   []byte(`{"model":"deepseek-v4-pro-think","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
		IsStream: true,
	})
	if err != nil {
		t.Fatalf("ForwardDeepSeekWeb failed: %v", err)
	}
	if !sawCookie || !sawPow {
		t.Fatalf("expected Cookie & PoW header sent upstream (sawCookie=%v sawPow=%v)", sawCookie, sawPow)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "DEEPSEEK_WEB_OK") || !strings.Contains(body, "reasoning_content") || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("unexpected SSE output: %s", body)
	}
}
