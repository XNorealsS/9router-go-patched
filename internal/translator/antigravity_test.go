package translator_test

import (
	"encoding/json"
	"strings"
	"testing"

	"9router/proxy/internal/translator"
)

func TestCloakAntigravityRequest_RenamesAndInjectsDecoys(t *testing.T) {
	req := &translator.GeminiRequest{
		Contents: []translator.GeminiContent{
			{
				Role: "model",
				Parts: []translator.GeminiPart{
					{FunctionCall: &translator.GeminiFunctionCall{Name: "execute_code", Args: map[string]any{"code": "ls"}}},
				},
			},
			{
				Role: "user",
				Parts: []translator.GeminiPart{
					{FunctionResponse: &translator.GeminiFunctionResp{Name: "execute_code"}},
				},
			},
		},
		Tools: []translator.GeminiTool{
			{
				FunctionDeclarations: []translator.GeminiFunctionDecl{
					{Name: "execute_code", Description: "Run code"},
					{Name: "run_command", Description: "Native tool"},
				},
			},
		},
	}

	cloaked, toolMap := translator.CloakAntigravityRequest(req, "")
	if cloaked == nil {
		t.Fatal("expected cloaked request, got nil")
	}

	// execute_code should be renamed to execute_code_ide
	if toolMap["execute_code_ide"] != "execute_code" {
		t.Errorf("expected toolMap[execute_code_ide] = execute_code, got %s", toolMap["execute_code_ide"])
	}

	// Check function call in history was renamed
	if cloaked.Contents[0].Parts[0].FunctionCall.Name != "execute_code_ide" {
		t.Errorf("expected contents functionCall renamed to execute_code_ide, got %s", cloaked.Contents[0].Parts[0].FunctionCall.Name)
	}
	if cloaked.Contents[1].Parts[0].FunctionResponse.Name != "execute_code_ide" {
		t.Errorf("expected contents functionResponse renamed to execute_code_ide, got %s", cloaked.Contents[1].Parts[0].FunctionResponse.Name)
	}

	// Check 21 decoy tools injected
	if len(cloaked.Tools) == 0 || len(cloaked.Tools[0].FunctionDeclarations) < 20 {
		t.Errorf("expected >= 20 function declarations including decoys, got %d", len(cloaked.Tools[0].FunctionDeclarations))
	}
}

func TestUncloakToolName(t *testing.T) {
	toolMap := map[string]string{
		"execute_code_ide": "execute_code",
		"custom_tool_ide":  "custom_tool",
	}

	if un := translator.UncloakToolName("execute_code_ide", toolMap); un != "execute_code" {
		t.Errorf("expected execute_code, got %s", un)
	}
	if un := translator.UncloakToolName("run_command", toolMap); un != "run_command" {
		t.Errorf("expected run_command unchanged, got %s", un)
	}
	if un := translator.UncloakToolName("other_ide", nil); un != "other" {
		t.Errorf("expected other (suffix stripped), got %s", un)
	}
}

func TestAntigravityImageModelAndConfig(t *testing.T) {
	if !translator.IsAntigravityImageModel("gemini-3.1-flash-image") {
		t.Error("expected gemini-3.1-flash-image to be image model")
	}
	if !translator.IsAntigravityImageModel("imagen-3.0-generate-002") {
		t.Error("expected imagen-3.0-generate-002 to be image model")
	}
	if translator.IsAntigravityImageModel("gemini-3-flash") {
		t.Error("expected gemini-3-flash NOT to be image model")
	}

	clean, ratio := translator.ParseImageConfig("gemini-3.1-flash-image-16x9")
	if clean != "gemini-3.1-flash-image" || ratio != "16:9" {
		t.Errorf("expected (gemini-3.1-flash-image, 16:9), got (%s, %s)", clean, ratio)
	}

	clean2, ratio2 := translator.ParseImageConfig("gemini-3.1-flash-image-1024x768")
	if clean2 != "gemini-3.1-flash-image" || ratio2 != "4:3" {
		t.Errorf("expected (gemini-3.1-flash-image, 4:3), got (%s, %s)", clean2, ratio2)
	}
}

func TestWrapAntigravityImageRequest(t *testing.T) {
	reqBytes, err := translator.WrapAntigravityImageRequest("A cute cat", "", "proj-123", "gemini-3.1-flash-image", "16:9")
	if err != nil {
		t.Fatalf("WrapAntigravityImageRequest failed: %v", err)
	}
	if len(reqBytes) == 0 {
		t.Fatal("expected non-empty request bytes")
	}

	var req translator.AntigravityRequest
	if err := json.Unmarshal(reqBytes, &req); err != nil {
		t.Fatalf("unmarshal wrapper failed: %v", err)
	}
	if req.RequestType != "image_gen" {
		t.Errorf("expected requestType image_gen, got %s", req.RequestType)
	}
	if req.Project != "proj-123" {
		t.Errorf("expected project proj-123, got %s", req.Project)
	}
}

func TestNormalizeAntigravityModel(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"gemini-3.5-flash-high", "gemini-3.8-flash-high"},
		{"gemini-3.5-flash-medium", "gemini-3.8-flash-medium"},
		{"gemini-3.5-flash-extra-low", "gemini-3.8-flash-low"},
		{"gemini-3.1-pro-high", "gemini-4-hydrogen-high"},
		{"gemini-3-pro-high", "gemini-4-hydrogen-high"},
		{"gemini-3-pro-low", "gemini-4-hydrogen-low"},
		{"gemini-default", "gemini-3.8-flash-medium"},
		{"claude-sonnet-4-6", "claude-sonnet-5-5-medium"},
		{"gemini-3-flash", "gemini-3-flash"},
	}

	for _, tt := range tests {
		got := translator.NormalizeAntigravityModel(tt.input)
		if got != tt.expected {
			t.Errorf("NormalizeAntigravityModel(%q) = %q, expected %q", tt.input, got, tt.expected)
		}
	}
}

func TestAntigravityDecoyTools_NonEmptyProperties(t *testing.T) {
	for _, dt := range translator.AntigravityDecoyTools {
		params, ok := dt.Parameters.(map[string]any)
		if !ok {
			t.Fatalf("decoy tool %s has invalid parameters type", dt.Name)
		}
		props, ok := params["properties"].(map[string]any)
		if !ok || len(props) == 0 {
			t.Errorf("decoy tool %s has empty properties; Gemini will reject with 'Invalid tool parameters'", dt.Name)
		}
	}
}

func TestTranslateOpenAIToGemini_ClaudeCodeToolResponseMapping(t *testing.T) {
	body := []byte(`{
		"model": "antigravity/gemini-3.5-flash-high",
		"messages": [
			{
				"role": "assistant",
				"tool_calls": [
					{
						"id": "toolu_01ABC123",
						"type": "function",
						"function": {
							"name": "plugin:claude-mem:mcp-search",
							"arguments": "{\"query\":\"test\"}"
						}
					}
				]
			},
			{
				"role": "tool",
				"tool_call_id": "toolu_01ABC123",
				"content": "{\"results\":[]}"
			}
		],
		"tools": [
			{
				"type": "function",
				"function": {
					"name": "plugin:claude-mem:mcp-search",
					"description": "search memory",
					"parameters": {
						"type": "object",
						"properties": {
							"query": { "type": "string" }
						},
						"required": ["query"]
					}
				}
			}
		]
	}`)

	geminiJSON, err := translator.TranslateOpenAIToGemini(body)
	if err != nil {
		t.Fatalf("TranslateOpenAIToGemini failed: %v", err)
	}

	var req translator.GeminiRequest
	if err := json.Unmarshal(geminiJSON, &req); err != nil {
		t.Fatalf("unmarshal gemini request failed: %v", err)
	}

	if len(req.Contents) != 2 {
		t.Fatalf("expected 2 contents, got %d", len(req.Contents))
	}

	// Tool response part must have exact name "plugin:claude-mem:mcp-search"
	respPart := req.Contents[1].Parts[0]
	if respPart.FunctionResponse == nil {
		t.Fatal("expected functionResponse part")
	}
	if respPart.FunctionResponse.Name != "plugin:claude-mem:mcp-search" {
		t.Errorf("expected functionResponse name 'plugin:claude-mem:mcp-search', got %q", respPart.FunctionResponse.Name)
	}
}

func TestStripCompetitivePrompts(t *testing.T) {
	req := &translator.GeminiRequest{
		SystemInstruction: &translator.GeminiContent{
			Role: "user",
			Parts: []translator.GeminiPart{
				{Text: "You are a Claude agent, built on Anthropic's Claude Agent SDK. Solve this task."},
			},
		},
		Contents: []translator.GeminiContent{
			{
				Role: "user",
				Parts: []translator.GeminiPart{
					{Text: "You are a Claude agent, built on Anthropic's Claude Agent SDK. Do something."},
				},
			},
		},
	}

	stripped := translator.StripCompetitivePrompts(req)
	if strings.Contains(stripped.SystemInstruction.Parts[0].Text, "Anthropic's Claude Agent SDK") {
		t.Errorf("expected competitive prompt removed from systemInstruction, got %s", stripped.SystemInstruction.Parts[0].Text)
	}
	if !strings.Contains(stripped.Contents[0].Parts[0].Text, "Anthropic's Claude Agent SDK") {
		t.Errorf("expected user contents preserved intact, got %s", stripped.Contents[0].Parts[0].Text)
	}
}

func TestNormalizeAntigravityModel_AllSynonymsValid(t *testing.T) {
	validBackendModels := map[string]bool{
		"gemini-3-flash":         true,
		"gemini-3.8-flash-low":   true,
		"gemini-3.8-flash-medium": true,
		"gemini-3.8-flash-high":  true,
		"gemini-3.5-flash-lite":  true,
		"gemini-3.1-flash-lite":  true,
		"gemini-3.1-flash-image": true,
		"gemini-4-argon-medium":  true,
		"gemini-4-hydrogen-low":  true,
		"gemini-4-hydrogen-medium": true,
		"gemini-4-hydrogen-high": true,
		"gemini-4-neon-medium":   true,
		"claude-sonnet-5-5-low":  true,
		"claude-sonnet-5-5-medium": true,
		"claude-sonnet-5-5-high": true,
		"claude-opus-5-5-low":    true,
		"claude-opus-5-5-medium": true,
		"claude-opus-5-5-high":   true,
		"gpt-oss-120b-medium":    true,
	}

	for alias, targetModel := range translator.AntigravityModelSynonyms {
		if !validBackendModels[targetModel] {
			t.Errorf("Antigravity synonym %q maps to invalid upstream model %q (will cause 404)", alias, targetModel)
		}
	}
}

func TestTranslateClaudeToOpenAI_DocumentPDFBlock(t *testing.T) {
	claudeReq := `{
		"model": "antigravity/gemini-3-flash",
		"messages": [{
			"role": "user",
			"content": [
				{"type": "text", "text": "Summarize this PDF"},
				{"type": "document", "source": {"type": "base64", "media_type": "application/pdf", "data": "JVBERi0xLjQ="}}
			]
		}]
	}`
	openaiBytes, err := translator.TranslateClaudeToOpenAI([]byte(claudeReq))
	if err != nil {
		t.Fatalf("TranslateClaudeToOpenAI failed: %v", err)
	}
	if !strings.Contains(string(openaiBytes), "data:application/pdf;base64,JVBERi0xLjQ=") {
		t.Errorf("expected PDF document block translated to OpenAI file block, got %s", openaiBytes)
	}
}

func TestTranslateGeminiChunkToOpenAI_IncrementsToolCallIndices(t *testing.T) {
	state := &translator.GeminiStreamState{}
	chunk := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"fn_a","args":{"x":1}}},{"functionCall":{"name":"fn_b","args":{"y":2}}}]}}]}`)
	out, err := translator.TranslateGeminiChunkToOpenAI(chunk, state)
	if err != nil {
		t.Fatalf("TranslateGeminiChunkToOpenAI failed: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `"index":0`) || !strings.Contains(s, `"index":1`) {
		t.Errorf("expected tool_call indices 0 and 1, got %s", s)
	}
}

func TestTranslateOpenAIToGemini_CoalescesParallelToolResults(t *testing.T) {
	body := []byte(`{
		"model": "gemini-3-flash",
		"messages": [
			{"role": "system", "content": "Be brief."},
			{"role": "user", "content": "Run two tools"},
			{
				"role": "assistant",
				"tool_calls": [
					{"id": "call_1", "type": "function", "function": {"name": "alpha", "arguments": "{\"x\":1}"}},
					{"id": "call_2", "type": "function", "function": {"name": "beta", "arguments": "{\"y\":2}"}}
				]
			},
			{"role": "tool", "tool_call_id": "call_1", "content": "{\"ok\":1}"},
			{"role": "tool", "tool_call_id": "call_2", "content": "{\"ok\":2}"}
		]
	}`)

	geminiJSON, err := translator.TranslateOpenAIToGemini(body)
	if err != nil {
		t.Fatalf("TranslateOpenAIToGemini failed: %v", err)
	}

	raw := string(geminiJSON)
	if !strings.Contains(raw, `"systemInstruction"`) {
		t.Errorf("expected camelCase systemInstruction field, got %s", raw)
	}
	if strings.Contains(raw, `"system_instruction"`) {
		t.Errorf("unexpected snake_case system_instruction field in %s", raw)
	}

	var req translator.GeminiRequest
	if err := json.Unmarshal(geminiJSON, &req); err != nil {
		t.Fatalf("unmarshal gemini request failed: %v", err)
	}
	if len(req.Contents) != 3 {
		t.Fatalf("expected 3 coalesced contents (user, model, user), got %d", len(req.Contents))
	}
	if req.Contents[2].Role != "user" || len(req.Contents[2].Parts) != 2 {
		t.Fatalf("expected third turn to be user with 2 functionResponse parts, got role=%q parts=%d", req.Contents[2].Role, len(req.Contents[2].Parts))
	}
	if req.Contents[2].Parts[0].FunctionResponse == nil || req.Contents[2].Parts[0].FunctionResponse.Name != "alpha" {
		t.Errorf("expected first functionResponse alpha, got %+v", req.Contents[2].Parts[0].FunctionResponse)
	}
	if req.Contents[2].Parts[1].FunctionResponse == nil || req.Contents[2].Parts[1].FunctionResponse.Name != "beta" {
		t.Errorf("expected second functionResponse beta, got %+v", req.Contents[2].Parts[1].FunctionResponse)
	}
}

func TestTranslateGeminiChunkToOpenAI_FinishReasonToolCalls(t *testing.T) {
	state := &translator.GeminiStreamState{}
	chunk1 := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"fn_a","args":{"x":1}}}]}}]}`)
	if _, err := translator.TranslateGeminiChunkToOpenAI(chunk1, state); err != nil {
		t.Fatalf("chunk1 failed: %v", err)
	}

	chunk2 := []byte(`{"candidates":[{"finishReason":"STOP"}]}`)
	out, err := translator.TranslateGeminiChunkToOpenAI(chunk2, state)
	if err != nil {
		t.Fatalf("chunk2 failed: %v", err)
	}
	if !strings.Contains(string(out), `"finish_reason":"tool_calls"`) {
		t.Errorf("expected finish_reason tool_calls after functionCall, got %s", string(out))
	}
}

func TestTranslateOpenAIToClaudeStreamSession_DeterministicToolBlockOrder(t *testing.T) {
	for _, sessionKey := range []string{"det-0", "det-1", "det-2", "det-3", "det-4"} {
		defer translator.ClearStreamState(sessionKey)
		c1 := []byte(`{"id":"cmpl-1","model":"claude-sonnet-4-6","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_0","type":"function","function":{"name":"alpha","arguments":"{\"a\":1}"}},{"index":1,"id":"call_1","type":"function","function":{"name":"beta","arguments":"{\"b\":2}"}}]}}]}`)
		if _, err := translator.TranslateOpenAIToClaudeStreamSession(sessionKey, c1); err != nil {
			t.Fatalf("c1 failed: %v", err)
		}
		c2 := []byte(`{"id":"cmpl-1","model":"claude-sonnet-4-6","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
		out, err := translator.TranslateOpenAIToClaudeStreamSession(sessionKey, c2)
		if err != nil {
			t.Fatalf("c2 failed: %v", err)
		}
		s := string(out)
		stop0 := strings.Index(s, `"type":"content_block_stop"`)
		delta1 := strings.Index(s, `"index":1`)
		if stop0 < 0 || delta1 < 0 || stop0 > delta1 {
			t.Fatalf("expected tool block 0 closed before block 1 delta on %s, got %s", sessionKey, s)
		}
	}
}




