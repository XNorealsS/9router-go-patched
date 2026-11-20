package translator

import (
	"context"
	"encoding/json"
	"testing"
)

// --- Context-based Usage ---

func TestContextUsage(t *testing.T) {
	ctx := context.Background()
	ctx = WithUsageCapture(ctx)

	if u := GetAndClearUsage(ctx); u != nil {
		t.Errorf("expected nil initially, got %#v", u)
	}

	SetUsage(ctx, &OpenAIUsage{PromptTokens: 10, CompletionTokens: 20})

	u := GetAndClearUsage(ctx)
	if u == nil {
		t.Fatal("expected non-nil usage")
	}
	if u.PromptTokens != 10 || u.CompletionTokens != 20 {
		t.Errorf("got %#v", u)
	}

	if u2 := GetAndClearUsage(ctx); u2 != nil {
		t.Errorf("expected nil after clear, got %#v", u2)
	}
}

// --- SetLastUsage / GetAndClearLastUsage ---

func TestGetAndClearLastUsage(t *testing.T) {
	// Clear residue from other tests that may have set lastUsage
	GetAndClearLastUsage()

	// No usage set
	if u := GetAndClearLastUsage(); u != nil {
		t.Errorf("expected nil initially, got %#v", u)
	}

	// Set and retrieve
	SetLastUsage(&OpenAIUsage{PromptTokens: 10, CompletionTokens: 20, CachedTokens: 5})
	u := GetAndClearLastUsage()
	if u == nil {
		t.Fatal("expected non-nil usage")
	}
	if u.PromptTokens != 10 || u.CompletionTokens != 20 || u.CachedTokens != 5 {
		t.Errorf("got %#v", u)
	}

	// Cleared after get
	if u2 := GetAndClearLastUsage(); u2 != nil {
		t.Errorf("expected nil after clear, got %#v", u2)
	}
}

// --- GetStreamUsage ---

// --- Cached tokens from provider usage ---

func TestParseClaudeUsage(t *testing.T) {
	u := ParseClaudeUsage([]byte(`{"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":6,"cache_creation_input_tokens":2}}`))
	if u == nil {
		t.Fatal("expected parsed usage")
	}
	if u.PromptTokens != 10 || u.CompletionTokens != 4 || u.CachedTokens != 6 || u.CacheCreationInputTokens != 2 {
		t.Errorf("got %#v", u)
	}
	// Non-Claude body (no usage) → nil, never a nil deref.
	if ParseClaudeUsage([]byte(`{}`)) != nil {
		t.Error("expected nil for body without usage")
	}
	// OpenAI-format usage must NOT be misread as Claude (would stamp all-zero usage).
	if ParseClaudeUsage([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":50}}`)) != nil {
		t.Error("expected nil for OpenAI-format usage")
	}
}

func TestParseResponseUsage(t *testing.T) {
	// Claude format
	u := ParseResponseUsage([]byte(`{"usage":{"input_tokens":10,"output_tokens":4,"cache_read_input_tokens":6}}`))
	if u == nil || u.PromptTokens != 10 || u.CompletionTokens != 4 || u.CachedTokens != 6 {
		t.Errorf("claude format: got %#v", u)
	}
	// OpenAI format (the !translate path also serves /v1/chat/completions bodies)
	u = ParseResponseUsage([]byte(`{"usage":{"prompt_tokens":100,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":30}}}`))
	if u == nil || u.PromptTokens != 100 || u.CompletionTokens != 40 || u.GetCachedTokens() != 30 {
		t.Errorf("openai format: got %#v", u)
	}
	// No usage → nil
	if ParseResponseUsage([]byte(`{}`)) != nil {
		t.Error("expected nil for body without usage")
	}
}

func TestTranslateGeminiResponseToOpenAI_cachedTokens(t *testing.T) {
	geminiBody, _ := json.Marshal(map[string]interface{}{
		"candidates": []map[string]interface{}{{
			"content": map[string]interface{}{"parts": []map[string]interface{}{{"text": "hi"}}},
		}},
		"usageMetadata": map[string]interface{}{
			"promptTokenCount":     100,
			"candidatesTokenCount": 5,
			"cachedContentToken":   90,
		},
	})
	out, usage, err := TranslateGeminiResponseToOpenAI(geminiBody)
	if err != nil {
		t.Fatalf("translate error: %v", err)
	}
	if usage.CachedTokens != 90 {
		t.Errorf("expected CachedTokens=90, got %d", usage.CachedTokens)
	}
	// The OpenAI usage map must carry cached_tokens so the OpenAI→Claude
	// double-translation preserves it.
	var parsed struct {
		Usage map[string]interface{} `json:"usage"`
	}
	if json.Unmarshal(out, &parsed) != nil {
		t.Fatal("expected parseable output")
	}
	if ct, ok := parsed.Usage["cached_tokens"].(float64); !ok || int(ct) != 90 {
		t.Errorf("expected cached_tokens=90 in output usage, got %v", parsed.Usage["cached_tokens"])
	}
}
