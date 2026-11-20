package translator

import (
	"strings"
	"testing"
)

// --- extractReasoningText ---

func TestExtractReasoningText(t *testing.T) {
	tests := []struct {
		name     string
		delta    OpenAIDelta
		expected string
	}{
		{"reasoning_content takes priority", OpenAIDelta{ReasoningContent: "think", Reasoning: "old"}, "think"},
		{"reasoning fallback", OpenAIDelta{Reasoning: "think"}, "think"},
		{"empty", OpenAIDelta{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractReasoningText(tt.delta)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

// --- formatSSE ---

func TestFormatSSE(t *testing.T) {
	event := map[string]any{
		"type": "content_block_delta",
		"delta": map[string]any{
			"type": "text_delta",
			"text": "hello",
		},
	}
	got := formatSSE(event)
	expected := `event: content_block_delta` + "\n" + `data: {"delta":{"text":"hello","type":"text_delta"},"type":"content_block_delta"}` + "\n\n"
	if got != expected {
		t.Errorf("formatSSE mismatch.\ngot:  %q\nwant: %q", got, expected)
	}
}

// --- TranslateOpenAIToClaudeStream edge cases ---

// --- TranslateOpenAIToClaude (non-stream) ---

func TestTranslateOpenAIToClaude(t *testing.T) {
	t.Run("basic text response", func(t *testing.T) {
		input := []byte(`{"id":"chatcmpl-123","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"Hello!"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
		out, _, err := TranslateOpenAIToClaude(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		s := string(out)
		if !strings.Contains(s, `"type":"message"`) {
			t.Errorf("expected type message, got: %s", s)
		}
		if !strings.Contains(s, `"type":"text"`) {
			t.Errorf("expected text content block, got: %s", s)
		}
		if !strings.Contains(s, `"text":"Hello!"`) {
			t.Errorf("expected Hello!, got: %s", s)
		}
		if !strings.Contains(s, `"stop_reason":"end_turn"`) {
			t.Errorf("expected end_turn, got: %s", s)
		}
		if !strings.Contains(s, `"input_tokens":10`) {
			t.Errorf("expected input_tokens 10, got: %s", s)
		}
	})

	t.Run("response with reasoning", func(t *testing.T) {
		input := []byte(`{"id":"chatcmpl-r","model":"o3-mini","choices":[{"index":0,"message":{"role":"assistant","content":"Answer","reasoning_content":"Let me think..."},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":20}}`)
		out, _, err := TranslateOpenAIToClaude(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		s := string(out)
		if !strings.Contains(s, `"type":"thinking"`) {
			t.Errorf("expected thinking block, got: %s", s)
		}
		if !strings.Contains(s, `"thinking":"Let me think..."`) {
			t.Errorf("expected reasoning text, got: %s", s)
		}
	})

	t.Run("response with tool calls", func(t *testing.T) {
		input := []byte(`{"id":"chatcmpl-t","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"/tmp\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
		out, _, err := TranslateOpenAIToClaude(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		s := string(out)
		if !strings.Contains(s, `"type":"tool_use"`) {
			t.Errorf("expected tool_use block, got: %s", s)
		}
		if !strings.Contains(s, `"name":"Read"`) {
			t.Errorf("expected tool name Read, got: %s", s)
		}
		if !strings.Contains(s, `"stop_reason":"tool_use"`) {
			t.Errorf("expected tool_use stop, got: %s", s)
		}
	})

	t.Run("empty response returns error", func(t *testing.T) {
		_, _, err := TranslateOpenAIToClaude([]byte(""))
		if err == nil {
			t.Error("expected error for empty input")
		}
	})

	t.Run("no choices returns error", func(t *testing.T) {
		_, _, err := TranslateOpenAIToClaude([]byte(`{"id":"chatcmpl-empty","model":"gpt-4o","choices":[]}`))
		if err == nil {
			t.Error("expected error for zero choices")
		}
	})

	t.Run("proxy_ prefix stripped", func(t *testing.T) {
		input := []byte(`{"id":"chatcmpl-proxy","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_p","type":"function","function":{"name":"proxy_Bash","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		out, _, err := TranslateOpenAIToClaude(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Contains(string(out), `"name":"proxy_Bash"`) {
			t.Errorf("proxy_ prefix should be stripped, got: %s", out)
		}
		if !strings.Contains(string(out), `"name":"Bash"`) {
			t.Errorf("expected Bash, got: %s", out)
		}
	})

	t.Run("usage returned to caller", func(t *testing.T) {
		input := []byte(`{"id":"chatcmpl-usage","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":42,"completion_tokens":8}}`)
		_, u, _ := TranslateOpenAIToClaude(input)
		if u == nil {
			t.Fatal("expected usage returned")
		}
		if u.PromptTokens != 42 || u.CompletionTokens != 8 {
			t.Errorf("usage mismatch: %#v", u)
		}
	})
}

// --- Provider alias edge: knownProviders used by TranslateOpenAIToClaudeStream ---

// --- TranslateOpenAIToClaude with reasoning_tokens in usage details ---

func TestTranslateOpenAIToClaude_CompletionTokensDetails(t *testing.T) {
	t.Run("captures reasoning_tokens from non-stream response", func(t *testing.T) {
		input := []byte(`{"id":"chatcmpl-detail","model":"o3-mini","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":99,"completion_tokens_details":{"reasoning_tokens":80}}}`)
		_, u, err := TranslateOpenAIToClaude(input)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if u == nil {
			t.Fatal("expected non-nil usage")
		}
		if u.PromptTokens != 10 {
			t.Errorf("expected prompt_tokens 10, got %d", u.PromptTokens)
		}
		if u.CompletionTokens != 99 {
			t.Errorf("expected completion_tokens 99, got %d", u.CompletionTokens)
		}
		if u.ReasoningTokens() != 80 {
			t.Errorf("expected reasoning_tokens 80, got %d", u.ReasoningTokens())
		}
	})
}
