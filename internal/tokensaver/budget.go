package tokensaver

import (
	"encoding/json"
	"fmt"
	"strings"
)

// BudgetStats summarizes adaptations performed to keep a request inside a target model window.
type BudgetStats struct {
	EstimatedTokensBefore int
	EstimatedTokensAfter  int
	SanitizedToolIDs      bool
	CompressedTools       int
	CondensedTurns        int
}

const tsDelimiter = "__ts__"

// FitContextBudget adapts an OpenAI-format JSON body to fit the destination provider's
// context window while keeping tool-call/tool-result turns atomic and preserving the
// initial user task goal. Non-Gemini providers also get Gemini thoughtSignature suffixes
// stripped from tool IDs cleanly.
func FitContextBudget(body []byte, targetProvider string, contextWindow, maxOutput int) ([]byte, BudgetStats) {
	var stats BudgetStats
	if contextWindow <= 0 {
		contextWindow = 128000
	}
	if maxOutput <= 0 || maxOutput >= contextWindow/2 {
		maxOutput = min(8192, contextWindow/4)
	}
	targetTokens := ((contextWindow - maxOutput) * 85) / 100
	if targetTokens < 1024 {
		targetTokens = contextWindow / 2
	}

	var req map[string]any
	if err := unmarshalAny(body, &req); err != nil {
		return body, stats
	}
	rawMsgs, ok := req["messages"].([]any)
	if !ok || len(rawMsgs) == 0 {
		return body, stats
	}

	mutated := false
	isGeminiTarget := targetProvider == "antigravity" || targetProvider == "gemini" || targetProvider == "gemini-cli"

	// Stage 1: Strip provider-foreign Gemini __ts__ suffixes when routing to non-Gemini providers.
	if !isGeminiTarget {
		if stripForeignThoughtSignatures(rawMsgs) {
			stats.SanitizedToolIDs = true
			mutated = true
		}
	}

	toolSchemaChars := 0
	if toolsRaw, ok := req["tools"]; ok {
		if b, err := json.Marshal(toolsRaw); err == nil {
			toolSchemaChars = len(b)
		}
	}

	stats.EstimatedTokensBefore = estimateMessagesTokens(rawMsgs, toolSchemaChars)
	if stats.EstimatedTokensBefore <= targetTokens {
		stats.EstimatedTokensAfter = stats.EstimatedTokensBefore
		if !mutated {
			return body, stats
		}
		out, err := json.Marshal(req)
		if err != nil {
			return body, stats
		}
		return out, stats
	}

	// Stage 2: Compress older verbose tool outputs while keeping the last 4 messages intact.
	protectedFrom := max(0, len(rawMsgs)-4)
	for i := 0; i < protectedFrom; i++ {
		msg, ok := rawMsgs[i].(map[string]any)
		if !ok {
			continue
		}
		if msg["role"] == "tool" {
			if c, ok := msg["content"].(string); ok && len(c) > MinCompressSize {
				compressed := CompressText(c)
				if len(compressed) > 1200 {
					compressed = compressed[:600] + "\n... [condensed for context budget] ...\n" + compressed[len(compressed)-300:]
				}
				if compressed != c {
					msg["content"] = compressed
					stats.CompressedTools++
					mutated = true
				}
			}
		}
	}

	est := estimateMessagesTokens(rawMsgs, toolSchemaChars)
	if est <= targetTokens {
		stats.EstimatedTokensAfter = est
		if !mutated {
			return body, stats
		}
		out, err := json.Marshal(req)
		if err != nil {
			return body, stats
		}
		return out, stats
	}

	// Stage 3: Turn-atomic sliding window preserving system instructions, first user task anchor,
	// and complete trailing turns (never splitting assistant tool_calls from tool results).
	fitted, condensed := atomicSlidingWindow(rawMsgs, targetTokens, toolSchemaChars)
	if condensed > 0 {
		req["messages"] = fitted
		stats.CondensedTurns = condensed
		mutated = true
		est = estimateMessagesTokens(fitted, toolSchemaChars)
	}
	stats.EstimatedTokensAfter = est
	if !mutated {
		return body, stats
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body, stats
	}
	return out, stats
}

// IsContextOverflowError reports whether an upstream HTTP error indicates the prompt
// exceeded the backend model's context length limit.
func IsContextOverflowError(status int, body []byte) bool {
	if status != 400 && status != 413 && status != 422 {
		return false
	}
	lower := strings.ToLower(string(body))
	return strings.Contains(lower, "context_length") ||
		strings.Contains(lower, "maximum context") ||
		strings.Contains(lower, "context window") ||
		strings.Contains(lower, "too many tokens") ||
		strings.Contains(lower, "token limit") ||
		strings.Contains(lower, "exceeds the limit") ||
		strings.Contains(lower, "input is too long")
}

func stripForeignThoughtSignatures(msgs []any) bool {
	changed := false
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := m["tool_call_id"].(string); ok {
			if idx := strings.Index(id, tsDelimiter); idx != -1 {
				m["tool_call_id"] = id[:idx]
				changed = true
			}
		}
		if calls, ok := m["tool_calls"].([]any); ok {
			for _, tcRaw := range calls {
				if tc, ok := tcRaw.(map[string]any); ok {
					if id, ok := tc["id"].(string); ok {
						if idx := strings.Index(id, tsDelimiter); idx != -1 {
							tc["id"] = id[:idx]
							changed = true
						}
					}
				}
			}
		}
	}
	return changed
}

func estimateMessagesTokens(msgs []any, extraChars int) int {
	chars := extraChars
	for _, m := range msgs {
		b, err := json.Marshal(m)
		if err == nil {
			chars += len(b)
		}
	}
	// ~3.5 chars per token is safer for mixed code/JSON payloads than /4
	return (chars*10 + 34) / 35
}

func atomicSlidingWindow(msgs []any, targetTokens, toolSchemaChars int) ([]any, int) {
	var systems []any
	var convo []any
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		if role == "system" || role == "developer" {
			systems = append(systems, raw)
		} else {
			convo = append(convo, raw)
		}
	}
	if len(convo) <= 2 {
		return msgs, 0
	}

	// Group into atomic turn units:
	// - assistant message with tool_calls merges with all immediately following role=="tool" results.
	type turnGroup struct {
		msgs      []any
		tokens    int
		toolCount int
	}
	var groups []turnGroup
	for i := 0; i < len(convo); i++ {
		m, _ := convo[i].(map[string]any)
		role, _ := m["role"].(string)
		hasCalls := false
		if calls, ok := m["tool_calls"].([]any); ok && len(calls) > 0 {
			hasCalls = true
		}
		grp := turnGroup{msgs: []any{convo[i]}}
		if role == "assistant" && hasCalls {
			grp.toolCount += len(m["tool_calls"].([]any))
			for i+1 < len(convo) {
				nextM, ok := convo[i+1].(map[string]any)
				if !ok || nextM["role"] != "tool" {
					break
				}
				i++
				grp.msgs = append(grp.msgs, convo[i])
			}
		}
		grp.tokens = estimateMessagesTokens(grp.msgs, 0)
		groups = append(groups, grp)
	}
	if len(groups) <= 2 {
		return msgs, 0
	}

	baseTokens := estimateMessagesTokens(systems, toolSchemaChars) + 64 // headroom for summary note
	// Always keep groups[0] (initial user goal) if it fits inside 25% of target budget
	keepFirst := false
	if groups[0].tokens < targetTokens/4 {
		keepFirst = true
		baseTokens += groups[0].tokens
	}

	startTailIdx := len(groups) - 1
	tailTokens := groups[startTailIdx].tokens
	startSearch := 0
	if keepFirst {
		startSearch = 1
	}
	for i := len(groups) - 2; i >= startSearch; i-- {
		if baseTokens+tailTokens+groups[i].tokens > targetTokens {
			break
		}
		tailTokens += groups[i].tokens
		startTailIdx = i
	}

	if startTailIdx <= startSearch {
		return msgs, 0
	}

	droppedTurns := startTailIdx - startSearch
	droppedTools := 0
	for i := startSearch; i < startTailIdx; i++ {
		droppedTools += groups[i].toolCount
	}

	out := make([]any, 0, len(msgs))
	out = append(out, systems...)
	if keepFirst {
		out = append(out, groups[0].msgs...)
	}
	summaryText := fmt.Sprintf("[Context Window Guard: %d earlier conversation turn(s) and %d tool invocation(s) condensed to fit target model budget while keeping original goal and active turns.]", droppedTurns, droppedTools)
	out = append(out, map[string]any{
		"role":    "user",
		"content": summaryText,
	})
	for i := startTailIdx; i < len(groups); i++ {
		out = append(out, groups[i].msgs...)
	}
	return out, droppedTurns
}
