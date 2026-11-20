package translator

import (
	"encoding/json"
	"testing"
)

func TestTranslateClaudeToOpenAI(t *testing.T) {
	t.Run("basic translation with system prompt", func(t *testing.T) {
		claudeJSON := []byte(`{
			"model": "claude-3-5-sonnet",
			"messages": [
				{"role": "user", "content": "hello"}
			],
			"system": "System instructions",
			"temperature": 0.7,
			"max_tokens": 1000
		}`)
		openaiJSON, err := TranslateClaudeToOpenAI(claudeJSON)
		if err != nil {
			t.Fatalf("failed to translate: %v", err)
		}

		// Verify structure
		var oreq map[string]interface{}
		if err := json.Unmarshal(openaiJSON, &oreq); err != nil {
			t.Fatalf("failed to parse output: %v", err)
		}

		if oreq["model"] != "claude-3-5-sonnet" {
			t.Errorf("expected model 'claude-3-5-sonnet', got %v", oreq["model"])
		}

		if oreq["temperature"] != 0.7 {
			t.Errorf("expected temperature 0.7, got %v", oreq["temperature"])
		}

		if oreq["max_tokens"] != float64(1000) {
			t.Errorf("expected max_tokens 1000, got %v", oreq["max_tokens"])
		}

		messages, ok := oreq["messages"].([]interface{})
		if !ok {
			t.Fatalf("messages is not an array")
		}

		if len(messages) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(messages))
		}

		sysMsg := messages[0].(map[string]interface{})
		if sysMsg["role"] != "system" || sysMsg["content"] != "System instructions" {
			t.Errorf("unexpected system message: %v", sysMsg)
		}

		userMsg := messages[1].(map[string]interface{})
		if userMsg["role"] != "user" || userMsg["content"] != "hello" {
			t.Errorf("unexpected user message: %v", userMsg)
		}
	})

	t.Run("system prompt array content", func(t *testing.T) {
		claudeJSON := []byte(`{
			"model": "claude-3-5-sonnet",
			"messages": [
				{"role": "user", "content": "hello"}
			],
			"system": [
				{"type": "text", "text": "System instructions part 1"},
				{"type": "text", "text": "System instructions part 2"}
			]
		}`)
		openaiJSON, err := TranslateClaudeToOpenAI(claudeJSON)
		if err != nil {
			t.Fatalf("failed to translate: %v", err)
		}

		var oreq map[string]interface{}
		_ = json.Unmarshal(openaiJSON, &oreq)

		messages := oreq["messages"].([]interface{})
		sysMsg := messages[0].(map[string]interface{})
		if sysMsg["role"] != "system" {
			t.Errorf("expected system message, got role %v", sysMsg["role"])
		}

		expectedContent := "System instructions part 1\nSystem instructions part 2"
		if sysMsg["content"] != expectedContent {
			t.Errorf("expected content %q, got %q", expectedContent, sysMsg["content"])
		}
	})

	t.Run("mid-conversation system messages", func(t *testing.T) {
		claudeJSON := []byte(`{
			"model": "claude-3-5-sonnet",
			"messages": [
				{"role": "user", "content": "hello"},
				{"role": "system", "content": "Mid-conversation instructions"},
				{"role": "assistant", "content": "hi"}
			]
		}`)
		openaiJSON, err := TranslateClaudeToOpenAI(claudeJSON)
		if err != nil {
			t.Fatalf("failed to translate: %v", err)
		}

		var oreq map[string]interface{}
		_ = json.Unmarshal(openaiJSON, &oreq)

		messages := oreq["messages"].([]interface{})
		if len(messages) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(messages))
		}

		userMsg := messages[0].(map[string]interface{})
		midSysMsg := messages[1].(map[string]interface{})
		assistantMsg := messages[2].(map[string]interface{})

		if userMsg["role"] != "user" || userMsg["content"] != "hello" {
			t.Errorf("unexpected message 1: %v", userMsg)
		}

		// Mid-conversation system prompt is converted to user role wrapped in <instructions>
		if midSysMsg["role"] != "user" {
			t.Errorf("expected mid-conversation system message to map to role 'user', got %v", midSysMsg["role"])
		}
		expectedContent := "<instructions>\nMid-conversation instructions\n</instructions>"
		if midSysMsg["content"] != expectedContent {
			t.Errorf("expected content %q, got %q", expectedContent, midSysMsg["content"])
		}

		if assistantMsg["role"] != "assistant" || assistantMsg["content"] != "hi" {
			t.Errorf("unexpected message 3: %v", assistantMsg)
		}
	})

	t.Run("content blocks - text and image", func(t *testing.T) {
		claudeJSON := []byte(`{
			"model": "claude-3-5-sonnet",
			"messages": [
				{
					"role": "user",
					"content": [
						{"type": "text", "text": "look at this image"},
						{
							"type": "image",
							"source": {
								"type": "base64",
								"media_type": "image/jpeg",
								"data": "SGVsbG8="
							}
						}
					]
				}
			]
		}`)
		openaiJSON, err := TranslateClaudeToOpenAI(claudeJSON)
		if err != nil {
			t.Fatalf("failed to translate: %v", err)
		}

		var oreq map[string]interface{}
		_ = json.Unmarshal(openaiJSON, &oreq)

		messages := oreq["messages"].([]interface{})
		userMsg := messages[0].(map[string]interface{})

		contentBlocks, ok := userMsg["content"].([]interface{})
		if !ok {
			t.Fatalf("expected content blocks array, got %v", userMsg["content"])
		}

		if len(contentBlocks) != 2 {
			t.Fatalf("expected 2 content blocks, got %d", len(contentBlocks))
		}

		textBlock := contentBlocks[0].(map[string]interface{})
		if textBlock["type"] != "text" || textBlock["text"] != "look at this image" {
			t.Errorf("unexpected text block: %v", textBlock)
		}

		imageBlock := contentBlocks[1].(map[string]interface{})
		if imageBlock["type"] != "image_url" {
			t.Errorf("expected type 'image_url', got %v", imageBlock["type"])
		}

		imageUrl, ok := imageBlock["image_url"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected image_url block, got %v", imageBlock["image_url"])
		}

		expectedUrl := "data:image/jpeg;base64,SGVsbG8="
		if imageUrl["url"] != expectedUrl {
			t.Errorf("expected url %q, got %q", expectedUrl, imageUrl["url"])
		}
	})

	t.Run("tool use and results", func(t *testing.T) {
		claudeJSON := []byte(`{
			"model": "claude-3-5-sonnet",
			"tools": [
				{
					"name": "get_weather",
					"description": "Get the current weather",
					"input_schema": {
						"type": "object",
						"properties": {
							"location": {"type": "string"}
						},
						"required": ["location"]
					}
				}
			],
			"messages": [
				{
					"role": "user",
					"content": "What is the weather in Paris?"
				},
				{
					"role": "assistant",
					"content": [
						{"type": "text", "text": "Let me check that."},
						{
							"type": "tool_use",
							"id": "toolu_1",
							"name": "get_weather",
							"input": {"location": "Paris"}
						}
					]
				},
				{
					"role": "user",
					"content": [
						{
							"type": "tool_result",
							"tool_use_id": "toolu_1",
							"content": "Sunny, 22°C"
						}
					]
				}
			]
		}`)

		openaiJSON, err := TranslateClaudeToOpenAI(claudeJSON)
		if err != nil {
			t.Fatalf("failed to translate: %v", err)
		}

		var oreq map[string]interface{}
		_ = json.Unmarshal(openaiJSON, &oreq)

		// Verify tools
		tools, ok := oreq["tools"].([]interface{})
		if !ok || len(tools) != 1 {
			t.Fatalf("expected 1 tool, got %v", oreq["tools"])
		}

		tool := tools[0].(map[string]interface{})
		if tool["type"] != "function" {
			t.Errorf("expected tool type 'function', got %v", tool["type"])
		}

		fn := tool["function"].(map[string]interface{})
		if fn["name"] != "get_weather" || fn["description"] != "Get the current weather" {
			t.Errorf("unexpected function metadata: %v", fn)
		}

		// Verify messages
		messages := oreq["messages"].([]interface{})
		if len(messages) != 3 {
			t.Fatalf("expected 3 messages, got %d", len(messages))
		}

		// 1. User question
		userMsg := messages[0].(map[string]interface{})
		if userMsg["role"] != "user" || userMsg["content"] != "What is the weather in Paris?" {
			t.Errorf("unexpected message 1: %v", userMsg)
		}

		// 2. Assistant tool call
		assistantMsg := messages[1].(map[string]interface{})
		if assistantMsg["role"] != "assistant" || assistantMsg["content"] != "Let me check that." {
			t.Errorf("unexpected assistant content: %v", assistantMsg)
		}

		toolCalls, ok := assistantMsg["tool_calls"].([]interface{})
		if !ok || len(toolCalls) != 1 {
			t.Fatalf("expected 1 tool_call, got %v", assistantMsg["tool_calls"])
		}

		toolCall := toolCalls[0].(map[string]interface{})
		if toolCall["id"] != "toolu_1" || toolCall["type"] != "function" {
			t.Errorf("unexpected tool_call structure: %v", toolCall)
		}

		callFn := toolCall["function"].(map[string]interface{})
		if callFn["name"] != "get_weather" {
			t.Errorf("expected function name 'get_weather', got %v", callFn["name"])
		}
		var argsMap map[string]interface{}
		if err := json.Unmarshal([]byte(callFn["arguments"].(string)), &argsMap); err != nil {
			t.Fatalf("failed to unmarshal arguments: %v", err)
		}
		if argsMap["location"] != "Paris" {
			t.Errorf("expected location to be 'Paris', got %v", argsMap["location"])
		}

		// 3. Tool response
		toolResultMsg := messages[2].(map[string]interface{})
		if toolResultMsg["role"] != "tool" {
			t.Errorf("expected message 3 to be a 'tool' message, got role %v", toolResultMsg["role"])
		}
		if toolResultMsg["tool_call_id"] != "toolu_1" || toolResultMsg["content"] != "Sunny, 22°C" {
			t.Errorf("unexpected tool response message: %v", toolResultMsg)
		}
	})

	t.Run("missing tool responses fix", func(t *testing.T) {
		// Assistant message has tool_calls but no following tool response
		claudeJSON := []byte(`{
			"model": "claude-3-5-sonnet",
			"messages": [
				{
					"role": "assistant",
					"content": [
						{
							"type": "tool_use",
							"id": "toolu_1",
							"name": "get_weather",
							"input": {"location": "Paris"}
						}
					]
				}
			]
		}`)

		openaiJSON, err := TranslateClaudeToOpenAI(claudeJSON)
		if err != nil {
			t.Fatalf("failed to translate: %v", err)
		}

		var oreq map[string]interface{}
		_ = json.Unmarshal(openaiJSON, &oreq)

		messages := oreq["messages"].([]interface{})
		// It should insert a mock tool response for toolu_1
		if len(messages) != 2 {
			t.Fatalf("expected 2 messages (assistant + fixed tool reply), got %d", len(messages))
		}

		toolResponse := messages[1].(map[string]interface{})
		if toolResponse["role"] != "tool" || toolResponse["tool_call_id"] != "toolu_1" || toolResponse["content"] != "[No response received]" {
			t.Errorf("unexpected inserted tool response: %v", toolResponse)
		}
	})

	t.Run("tool_choice variants", func(t *testing.T) {
		testCases := []struct {
			name       string
			toolChoice string
			expected   interface{}
		}{
			{"auto", `"auto"`, "auto"},
			{"any", `{"type":"any"}`, "required"},
			{"specific tool", `{"type":"tool","name":"get_weather"}`, map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "get_weather"}}},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				claudeJSON := []byte(`{
					"model": "claude-3-5-sonnet",
					"messages": [{"role": "user", "content": "hello"}],
					"tool_choice": ` + tc.toolChoice + `
				}`)
				openaiJSON, err := TranslateClaudeToOpenAI(claudeJSON)
				if err != nil {
					t.Fatalf("failed to translate: %v", err)
				}

				var oreq map[string]interface{}
				_ = json.Unmarshal(openaiJSON, &oreq)

				choiceVal := oreq["tool_choice"]
				switch exp := tc.expected.(type) {
				case string:
					if choiceVal != exp {
						t.Errorf("expected tool_choice %v, got %v", exp, choiceVal)
					}
				case map[string]interface{}:
					choiceMap, ok := choiceVal.(map[string]interface{})
					if !ok {
						t.Fatalf("expected tool_choice map, got %T", choiceVal)
					}
					fnMap := choiceMap["function"].(map[string]interface{})
					expFnMap := exp["function"].(map[string]interface{})
					if fnMap["name"] != expFnMap["name"] {
						t.Errorf("expected function name %v, got %v", expFnMap["name"], fnMap["name"])
					}
				}
			})
		}
	})
}
