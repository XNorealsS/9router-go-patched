package translator

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTranslateOpenAIToClaudeRequest_MessagesToolsAndCoalescing(t *testing.T) {
	openaiReq := `{
		"model": "minimax-m3",
		"stream": true,
		"messages": [
			{"role": "system", "content": "Follow schema strictly."},
			{"role": "user", "content": [
				{"type": "text", "text": "Inspect this"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,iVBORw0="}}
			]},
			{"role": "assistant", "reasoning_content": "checking", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "a", "arguments": "{\"x\":1}"}},
				{"id": "call_2", "type": "function", "function": {"name": "b", "arguments": "{\"y\":2}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "res1"},
			{"role": "tool", "tool_call_id": "call_2", "content": "res2"}
		],
		"tools": [
			{"type": "function", "function": {"name": "a", "description": "tool a", "parameters": {"type": "object"}}}
		],
		"tool_choice": "required"
	}`

	out, err := TranslateOpenAIToClaudeRequest([]byte(openaiReq))
	if err != nil {
		t.Fatalf("TranslateOpenAIToClaudeRequest failed: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal claude request: %v", err)
	}
	if parsed["system"] != "Follow schema strictly." {
		t.Errorf("unexpected system: %v", parsed["system"])
	}
	if mt, ok := parsed["max_tokens"].(float64); !ok || mt != 8192 {
		t.Errorf("expected default max_tokens=8192, got %v", parsed["max_tokens"])
	}
	tc, _ := parsed["tool_choice"].(map[string]any)
	if tc["type"] != "any" {
		t.Errorf("expected tool_choice type any, got %v", tc)
	}
	msgs := parsed["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 coalesced messages (user, assistant, user), got %d: %s", len(msgs), string(out))
	}
	lastUser := msgs[2].(map[string]any)
	if lastUser["role"] != "user" {
		t.Fatalf("expected final message role user, got %v", lastUser["role"])
	}
	if blocks := lastUser["content"].([]any); len(blocks) != 2 {
		t.Fatalf("expected 2 coalesced tool_result blocks, got %d", len(blocks))
	}
}

func TestTranslateClaudeResponseToOpenAI_ToolsAndUsage(t *testing.T) {
	claudeResp := `{
		"id": "msg_abc",
		"type": "message",
		"role": "assistant",
		"model": "minimax-m3",
		"content": [
			{"type": "thinking", "thinking": "step 1"},
			{"type": "text", "text": "Running tool"},
			{"type": "tool_use", "id": "toolu_1", "name": "lookup", "input": {"k": "v"}}
		],
		"stop_reason": "tool_use",
		"usage": {
			"input_tokens": 50,
			"output_tokens": 20,
			"cache_read_input_tokens": 12,
			"cache_creation_input_tokens": 5
		}
	}`

	out, usage, err := TranslateClaudeResponseToOpenAI([]byte(claudeResp))
	if err != nil {
		t.Fatalf("TranslateClaudeResponseToOpenAI failed: %v", err)
	}
	if usage == nil || usage.PromptTokens != 50 || usage.CompletionTokens != 20 || usage.CachedTokens != 12 || usage.CacheCreationInputTokens != 5 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
	s := string(out)
	if !strings.Contains(s, `"finish_reason":"tool_calls"`) || !strings.Contains(s, `"reasoning_content":"step 1"`) || !strings.Contains(s, `"name":"lookup"`) {
		t.Fatalf("unexpected OpenAI response JSON: %s", s)
	}
}

func TestTranslateClaudeStreamToOpenAI_FullLifecycle(t *testing.T) {
	var state ClaudeToOpenAIStreamState
	events := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"minimax-m3","usage":{"input_tokens":30,"cache_read_input_tokens":4}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi "}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_x","name":"run","input":{}}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":15}}`,
		`{"type":"message_stop"}`,
	}

	var combined strings.Builder
	for _, ev := range events {
		out, err := TranslateClaudeStreamToOpenAI([]byte(ev), &state)
		if err != nil {
			t.Fatalf("TranslateClaudeStreamToOpenAI error on %s: %v", ev, err)
		}
		combined.Write(out)
	}

	all := combined.String()
	for _, want := range []string{`"role":"assistant"`, `"content":"Hi "`, `"name":"run"`, `"arguments":"{\"a\":1}"`, `"finish_reason":"tool_calls"`, "data: [DONE]\n\n"} {
		if !strings.Contains(all, want) {
			t.Errorf("expected stream output to contain %q, got:\n%s", want, all)
		}
	}
	if state.Usage == nil || state.Usage.PromptTokens != 30 || state.Usage.CompletionTokens != 15 || state.Usage.CachedTokens != 4 {
		t.Fatalf("unexpected stream usage: %+v", state.Usage)
	}
}

func TestTranslateGeminiResponseToOpenAI_UniqueToolCallIDsAcrossTurns(t *testing.T) {
	resp := []byte(`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"read_file","args":{"path":"a.go"}}}]}}]}`)
	out1, _, err := TranslateGeminiResponseToOpenAI(resp)
	if err != nil {
		t.Fatalf("turn 1 failed: %v", err)
	}
	out2, _, err := TranslateGeminiResponseToOpenAI(resp)
	if err != nil {
		t.Fatalf("turn 2 failed: %v", err)
	}
	var p1, p2 OpenAIResponse
	_ = json.Unmarshal(out1, &p1)
	_ = json.Unmarshal(out2, &p2)
	id1 := p1.Choices[0].Message.ToolCalls[0].ID
	id2 := p2.Choices[0].Message.ToolCalls[0].ID
	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("expected unique tool call IDs across turns, got id1=%q id2=%q", id1, id2)
	}
}
