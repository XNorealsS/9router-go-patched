package tokensaver

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFitContextBudget_StripsForeignThoughtSigForNonGemini(t *testing.T) {
	body := []byte(`{
		"model":"deepseek-chat",
		"messages":[
			{"role":"user","content":"hi"},
			{"role":"assistant","tool_calls":[{"id":"call_1__ts__abc123sig","type":"function","function":{"name":"run","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1__ts__abc123sig","content":"ok"}
		]
	}`)
	out, st := FitContextBudget(body, "deepseek-web", 131072, 8192)
	if !st.SanitizedToolIDs {
		t.Fatal("expected SanitizedToolIDs=true for non-Gemini target")
	}
	if strings.Contains(string(out), "__ts__") {
		t.Fatalf("expected __ts__ suffix stripped, got %s", string(out))
	}
	if !strings.Contains(string(out), `"call_1"`) {
		t.Fatalf("expected clean call_1 ID preserved, got %s", string(out))
	}

	// Gemini target keeps __ts__ intact.
	outGem, stGem := FitContextBudget(body, "antigravity", 1048576, 65536)
	if stGem.SanitizedToolIDs || !strings.Contains(string(outGem), "__ts__abc123sig") {
		t.Fatalf("expected antigravity to preserve __ts__ signature")
	}
}

func TestFitContextBudget_AtomicSlidingWindowPreservesToolPairsAndGoal(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "You are helpful."},
		map[string]any{"role": "user", "content": "PRIMARY_GOAL_ANCHOR"},
	}
	big := strings.Repeat("verbose line output data ", 400)
	for i := 0; i < 6; i++ {
		msgs = append(msgs,
			map[string]any{"role": "user", "content": "step question " + big},
			map[string]any{
				"role": "assistant",
				"tool_calls": []any{
					map[string]any{"id": "tc_" + itoa(i), "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}},
				},
			},
			map[string]any{"role": "tool", "tool_call_id": "tc_" + itoa(i), "content": "tool out " + big},
		)
	}
	raw, _ := json.Marshal(map[string]any{"model": "test", "messages": msgs})

	out, st := FitContextBudget(raw, "kilo-gateway", 16000, 2000)
	if st.CondensedTurns == 0 || st.EstimatedTokensAfter > st.EstimatedTokensBefore {
		t.Fatalf("expected turns condensed cleanly, stats=%+v", st)
	}
	s := string(out)
	if !strings.Contains(s, "PRIMARY_GOAL_ANCHOR") {
		t.Fatal("expected initial user goal preserved as anchor")
	}
	if !strings.Contains(s, "Context Window Guard") {
		t.Fatal("expected condensed summary marker")
	}

	// Verify tool_call/tool pairing integrity in resulting messages slice.
	var parsed struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("unmarshal fitted: %v", err)
	}
	for i, m := range parsed.Messages {
		if m["role"] == "tool" {
			if i == 0 {
				t.Fatal("orphan tool message at index 0")
			}
			prev := parsed.Messages[i-1]
			if prev["role"] != "assistant" && prev["role"] != "tool" {
				t.Fatalf("orphan tool message preceded by role=%v", prev["role"])
			}
		}
	}
}
