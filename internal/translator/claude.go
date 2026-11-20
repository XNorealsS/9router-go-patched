package translator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type claudeOutMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// TranslateOpenAIToClaudeRequest converts an OpenAI Chat Completions JSON body
// into an Anthropic Messages (/v1/messages) JSON body.
func TranslateOpenAIToClaudeRequest(openaiBody []byte) ([]byte, error) {
	var oreq struct {
		Model           string          `json:"model"`
		Messages        json.RawMessage `json:"messages"`
		Temperature     *float64        `json:"temperature,omitempty"`
		MaxTokens       *int            `json:"max_tokens,omitempty"`
		TopP            *float64        `json:"top_p,omitempty"`
		Tools           json.RawMessage `json:"tools,omitempty"`
		ToolChoice      json.RawMessage `json:"tool_choice,omitempty"`
		ReasoningEffort string          `json:"reasoning_effort,omitempty"`
		Stream          bool            `json:"stream,omitempty"`
		Stop            json.RawMessage `json:"stop,omitempty"`
	}
	if err := json.Unmarshal(openaiBody, &oreq); err != nil {
		return nil, fmt.Errorf("parse OpenAI request: %w", err)
	}

	var msgs []struct {
		Role             string           `json:"role"`
		Content          any              `json:"content"`
		ToolCalls        []OpenAIToolCall `json:"tool_calls,omitempty"`
		ToolCallID       string           `json:"tool_call_id,omitempty"`
		ReasoningContent string           `json:"reasoning_content,omitempty"`
	}
	if len(oreq.Messages) > 0 {
		if err := json.Unmarshal(oreq.Messages, &msgs); err != nil {
			return nil, fmt.Errorf("parse OpenAI messages: %w", err)
		}
	}

	var systemParts []string
	var claudeMsgs []claudeOutMessage

	appendBlocks := func(role string, blocks []map[string]any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(claudeMsgs); n > 0 && claudeMsgs[n-1].Role == role {
			if prev, ok := claudeMsgs[n-1].Content.([]map[string]any); ok {
				claudeMsgs[n-1].Content = append(prev, blocks...)
				return
			}
		}
		claudeMsgs = append(claudeMsgs, claudeOutMessage{
			Role:    role,
			Content: blocks,
		})
	}

	for _, msg := range msgs {
		switch msg.Role {
		case "system", "developer":
			if s := strings.TrimSpace(extractContentString(msg.Content)); s != "" {
				systemParts = append(systemParts, s)
			}

		case "user":
			blocks := openAIContentToClaudeBlocks(msg.Content)
			if len(blocks) == 1 && blocks[0]["type"] == "text" {
				if n := len(claudeMsgs); n > 0 && claudeMsgs[n-1].Role == "user" {
					appendBlocks("user", blocks)
				} else if txt, ok := blocks[0]["text"].(string); ok {
					claudeMsgs = append(claudeMsgs, claudeOutMessage{Role: "user", Content: txt})
				}
			} else if len(blocks) > 0 {
				if n := len(claudeMsgs); n > 0 && claudeMsgs[n-1].Role == "user" {
					if prevStr, ok := claudeMsgs[n-1].Content.(string); ok {
						claudeMsgs[n-1].Content = append([]map[string]any{{"type": "text", "text": prevStr}}, blocks...)
					} else {
						appendBlocks("user", blocks)
					}
				} else {
					appendBlocks("user", blocks)
				}
			}

		case "assistant":
			var blocks []map[string]any
			if rc := strings.TrimSpace(msg.ReasoningContent); rc != "" {
				blocks = append(blocks, map[string]any{"type": "thinking", "thinking": msg.ReasoningContent})
			}
			blocks = append(blocks, openAIContentToClaudeBlocks(msg.Content)...)
			for _, tc := range msg.ToolCalls {
				var inputObj any = map[string]any{}
				if tc.Function.Arguments != "" {
					var parsed any
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &parsed); err == nil && parsed != nil {
						inputObj = parsed
					}
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Function.Name,
					"input": inputObj,
				})
			}
			appendBlocks("assistant", blocks)

		case "tool":
			contentStr := extractContentString(msg.Content)
			appendBlocks("user", []map[string]any{{
				"type":        "tool_result",
				"tool_use_id": msg.ToolCallID,
				"content":     contentStr,
			}})
		}
	}

	maxTokens := 8192
	if oreq.MaxTokens != nil && *oreq.MaxTokens > 0 {
		maxTokens = *oreq.MaxTokens
	}

	out := map[string]any{
		"model":      oreq.Model,
		"max_tokens": maxTokens,
		"messages":   claudeMsgs,
		"stream":     oreq.Stream,
	}
	if len(systemParts) > 0 {
		out["system"] = strings.Join(systemParts, "\n\n")
	}
	if oreq.Temperature != nil {
		out["temperature"] = *oreq.Temperature
	}
	if oreq.TopP != nil {
		out["top_p"] = *oreq.TopP
	}
	if len(oreq.Stop) > 0 {
		out["stop_sequences"] = oreq.Stop
	}

	if oreq.ReasoningEffort != "" {
		budget := effortToBudget(oreq.ReasoningEffort)
		if budget >= maxTokens {
			budget = maxTokens - 1024
		}
		if budget >= 1024 {
			out["thinking"] = map[string]any{
				"type":          "enabled",
				"budget_tokens": budget,
			}
		}
	}

	if len(oreq.Tools) > 0 {
		var openaiTools []OpenAITool
		if err := json.Unmarshal(oreq.Tools, &openaiTools); err == nil && len(openaiTools) > 0 {
			claudeTools := make([]ClaudeTool, 0, len(openaiTools))
			for _, t := range openaiTools {
				schema := t.Function.Parameters
				if len(schema) == 0 {
					schema = json.RawMessage(`{"type":"object","properties":{}}`)
				}
				claudeTools = append(claudeTools, ClaudeTool{
					Name:        t.Function.Name,
					Description: t.Function.Description,
					InputSchema: schema,
				})
			}
			out["tools"] = claudeTools
		}
	}

	if len(oreq.ToolChoice) > 0 {
		if tc := convertOpenAIToolChoiceToClaude(oreq.ToolChoice); tc != nil {
			out["tool_choice"] = tc
		}
	}

	return json.Marshal(out)
}

func openAIContentToClaudeBlocks(content any) []map[string]any {
	switch v := content.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []map[string]any{{"type": "text", "text": v}}
	case []any:
		var blocks []map[string]any
		for _, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := m["type"].(string)
			if blockType == "text" || blockType == "" {
				if txt, ok := m["text"].(string); ok && txt != "" {
					blocks = append(blocks, map[string]any{"type": "text", "text": txt})
					continue
				}
			}
			if img, ok := m["image_url"].(map[string]any); ok {
				if rawURL, ok := img["url"].(string); ok && rawURL != "" {
					if strings.HasPrefix(rawURL, "data:") {
						if semi := strings.Index(rawURL, ";"); semi > 5 {
							if comma := strings.Index(rawURL, ","); comma > semi {
								blocks = append(blocks, map[string]any{
									"type": "image",
									"source": map[string]any{
										"type":       "base64",
										"media_type": rawURL[5:semi],
										"data":       rawURL[comma+1:],
									},
								})
							}
						}
					} else if strings.HasPrefix(rawURL, "http://") || strings.HasPrefix(rawURL, "https://") {
						blocks = append(blocks, map[string]any{
							"type": "image",
							"source": map[string]any{
								"type": "url",
								"url":  rawURL,
							},
						})
					}
				}
			}
		}
		return blocks
	default:
		return nil
	}
}

func convertOpenAIToolChoiceToClaude(raw json.RawMessage) any {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return map[string]string{"type": "auto"}
		case "required":
			return map[string]string{"type": "any"}
		default:
			return nil
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Type == "function" && obj.Function.Name != "" {
		return map[string]string{
			"type": "tool",
			"name": obj.Function.Name,
		}
	}
	return nil
}

func mapClaudeStopReasonToOpenAI(stopReason string) string {
	switch stopReason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	default:
		return "stop"
	}
}

// TranslateClaudeResponseToOpenAI converts a non-streaming Anthropic Messages response
// JSON body into an OpenAI Chat Completions JSON response and extracts token usage.
func TranslateClaudeResponseToOpenAI(claudeResp []byte) ([]byte, *OpenAIUsage, error) {
	var msg struct {
		ID         string               `json:"id"`
		Model      string               `json:"model"`
		StopReason string               `json:"stop_reason"`
		Content    []ClaudeContentBlock `json:"content"`
		Usage      *struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(claudeResp, &msg); err != nil {
		return nil, nil, fmt.Errorf("parse Claude response: %w", err)
	}

	var textBuf strings.Builder
	var thinkingBuf strings.Builder
	var toolCalls []OpenAIToolCall

	for _, blk := range msg.Content {
		switch blk.Type {
		case "text":
			textBuf.WriteString(blk.Text)
		case "thinking":
			thinkingBuf.WriteString(blk.Thinking)
		case "tool_use":
			args := "{}"
			if len(blk.Input) > 0 {
				args = string(blk.Input)
			}
			toolCalls = append(toolCalls, OpenAIToolCall{
				ID:   blk.ID,
				Type: "function",
				Function: OpenAIFunctionCall{
					Name:      blk.Name,
					Arguments: args,
				},
			})
		}
	}

	finishReason := mapClaudeStopReasonToOpenAI(msg.StopReason)
	if len(toolCalls) > 0 && finishReason == "stop" {
		finishReason = "tool_calls"
	}

	message := map[string]any{
		"role":    "assistant",
		"content": textBuf.String(),
	}
	if thinkingBuf.Len() > 0 {
		message["reasoning_content"] = thinkingBuf.String()
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}

	var usage *OpenAIUsage
	if msg.Usage != nil {
		usage = &OpenAIUsage{
			PromptTokens:             msg.Usage.InputTokens,
			CompletionTokens:         msg.Usage.OutputTokens,
			CachedTokens:             msg.Usage.CacheReadInputTokens,
			CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
		}
	} else {
		usage = &OpenAIUsage{}
	}

	id := msg.ID
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   msg.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]any{
			"prompt_tokens":              usage.PromptTokens,
			"completion_tokens":          usage.CompletionTokens,
			"cached_tokens":              usage.CachedTokens,
			"cache_creation_input_tokens": usage.CacheCreationInputTokens,
			"total_tokens":               usage.PromptTokens + usage.CompletionTokens,
		},
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal OpenAI response: %w", err)
	}
	return out, usage, nil
}

// ClaudeToOpenAIStreamState tracks Anthropic SSE -> OpenAI SSE chunk translation state.
type ClaudeToOpenAIStreamState struct {
	MessageID       string
	Model           string
	RoleSent        bool
	DoneSent        bool
	BlockTypes      map[int]string
	ToolIdxByBlock  map[int]int
	NextToolIndex   int
	Usage           *OpenAIUsage
}

// TranslateClaudeStreamToOpenAI converts one Anthropic Messages SSE JSON data payload
// into zero or more OpenAI SSE `data: ...\n\n` events (and `data: [DONE]\n\n` on `message_stop`).
func TranslateClaudeStreamToOpenAI(ssePayload []byte, state *ClaudeToOpenAIStreamState) ([]byte, error) {
	trimmed := bytes.TrimSpace(ssePayload)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return nil, nil
	}
	var ev struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message *struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		ContentBlock *struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"content_block"`
		Delta *struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
		} `json:"delta"`
		Usage *struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(trimmed, &ev); err != nil {
		return nil, fmt.Errorf("parse Claude stream event: %w", err)
	}

	if state.BlockTypes == nil {
		state.BlockTypes = make(map[int]string)
	}
	if state.ToolIdxByBlock == nil {
		state.ToolIdxByBlock = make(map[int]int)
	}
	if state.Usage == nil {
		state.Usage = &OpenAIUsage{}
	}
	if state.MessageID == "" {
		state.MessageID = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}

	var buf bytes.Buffer
	writeChunk := func(delta map[string]any, finishReason any, includeUsage bool) {
		choice := map[string]any{
			"index":         0,
			"delta":         delta,
			"finish_reason": finishReason,
		}
		chunk := map[string]any{
			"id":      state.MessageID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   state.Model,
			"choices": []map[string]any{choice},
		}
		if includeUsage {
			chunk["usage"] = map[string]any{
				"prompt_tokens":               state.Usage.PromptTokens,
				"completion_tokens":           state.Usage.CompletionTokens,
				"cached_tokens":               state.Usage.CachedTokens,
				"cache_creation_input_tokens": state.Usage.CacheCreationInputTokens,
				"total_tokens":                state.Usage.PromptTokens + state.Usage.CompletionTokens,
			}
		}
		if b, err := json.Marshal(chunk); err == nil {
			buf.WriteString("data: ")
			buf.Write(b)
			buf.WriteString("\n\n")
		}
	}

	switch ev.Type {
	case "message_start":
		if ev.Message != nil {
			if ev.Message.ID != "" {
				state.MessageID = ev.Message.ID
			}
			if ev.Message.Model != "" {
				state.Model = ev.Message.Model
			}
			if ev.Message.Usage != nil {
				state.Usage.PromptTokens = ev.Message.Usage.InputTokens
				state.Usage.CompletionTokens = ev.Message.Usage.OutputTokens
				state.Usage.CachedTokens = ev.Message.Usage.CacheReadInputTokens
				state.Usage.CacheCreationInputTokens = ev.Message.Usage.CacheCreationInputTokens
			}
		}
		if !state.RoleSent {
			state.RoleSent = true
			writeChunk(map[string]any{"role": "assistant"}, nil, false)
		}

	case "content_block_start":
		if ev.ContentBlock != nil {
			state.BlockTypes[ev.Index] = ev.ContentBlock.Type
			if ev.ContentBlock.Type == "tool_use" {
				toolIdx := state.NextToolIndex
				state.NextToolIndex++
				state.ToolIdxByBlock[ev.Index] = toolIdx
				writeChunk(map[string]any{
					"tool_calls": []map[string]any{{
						"index": toolIdx,
						"id":    ev.ContentBlock.ID,
						"type":  "function",
						"function": map[string]any{
							"name":      ev.ContentBlock.Name,
							"arguments": "",
						},
					}},
				}, nil, false)
			}
		}

	case "content_block_delta":
		if ev.Delta != nil {
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					writeChunk(map[string]any{"content": ev.Delta.Text}, nil, false)
				}
			case "thinking_delta":
				if ev.Delta.Thinking != "" {
					writeChunk(map[string]any{"reasoning_content": ev.Delta.Thinking}, nil, false)
				}
			case "input_json_delta":
				if toolIdx, ok := state.ToolIdxByBlock[ev.Index]; ok && ev.Delta.PartialJSON != "" {
					writeChunk(map[string]any{
						"tool_calls": []map[string]any{{
							"index": toolIdx,
							"type":  "function",
							"function": map[string]any{
								"arguments": ev.Delta.PartialJSON,
							},
						}},
					}, nil, false)
				}
			}
		}

	case "message_delta":
		if ev.Usage != nil && ev.Usage.OutputTokens > 0 {
			state.Usage.CompletionTokens = ev.Usage.OutputTokens
		}
		if ev.Delta != nil && ev.Delta.StopReason != "" {
			writeChunk(map[string]any{}, mapClaudeStopReasonToOpenAI(ev.Delta.StopReason), true)
		}

	case "message_stop":
		if !state.DoneSent {
			state.DoneSent = true
			buf.WriteString("data: [DONE]\n\n")
		}
	}

	if buf.Len() == 0 {
		return nil, nil
	}
	return buf.Bytes(), nil
}
