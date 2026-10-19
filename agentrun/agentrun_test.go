package agentrun_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mind-vm/thinking-core/agentloop"
	"github.com/mind-vm/thinking-core/agentrun"
)

// The wire is camelCase throughout, and the loop's summary is not; a client
// reading `sessionId` off a `done` event is the whole reason Event exists.
func TestFromRunEvent_RecasesTheSummary(t *testing.T) {
	e := agentloop.RunEvent{Type: "done", Summary: &agentloop.RunSummary{SessionID: "s1", Steps: 3, DataBytesCarried: 42}}
	e.Summary.Tokens.Prompt, e.Summary.Tokens.Completion = 10, 5

	b, err := json.Marshal(agentrun.FromRunEvent(e))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{`"sessionId":"s1"`, `"dataBytesCarried":42`, `"tokens":{"prompt":10,"completion":5}`} {
		if !strings.Contains(got, want) {
			t.Errorf("wire event %s lacks %s", got, want)
		}
	}
	if strings.Contains(got, "session_id") {
		t.Errorf("wire event %s still carries the loop's snake_case", got)
	}
}

func TestFromRunEvent_KeepsPerCallTokens(t *testing.T) {
	got := agentrun.FromRunEvent(agentloop.RunEvent{
		Type: "execute_js_result", Content: "ok", Tool: "t", Args: map[string]any{"a": 1},
		Tokens: &agentloop.CallTokens{Prompt: 7, Completion: 2},
	})
	if got.Type != "execute_js_result" || got.Content != "ok" || got.Tool != "t" || got.Args["a"] != 1 {
		t.Fatalf("fields lost in translation: %+v", got)
	}
	if got.Tokens == nil || *got.Tokens != (agentrun.Tokens{Prompt: 7, Completion: 2}) {
		t.Fatalf("tokens = %+v", got.Tokens)
	}
	if got.Summary != nil {
		t.Fatalf("a non-terminal event grew a summary: %+v", got.Summary)
	}
}

func TestSummaryOf(t *testing.T) {
	res := agentloop.RunResult{RunID: "s1", Steps: 4, DataBytesCarried: 9}
	res.Tokens.Prompt, res.Tokens.Completion = 100, 20
	want := agentrun.Summary{SessionID: "s1", Steps: 4, Tokens: agentrun.Tokens{Prompt: 100, Completion: 20}, DataBytesCarried: 9}
	if got := agentrun.SummaryOf(res); got != want {
		t.Fatalf("SummaryOf = %+v, want %+v", got, want)
	}
}
