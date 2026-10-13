package agentloop_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mind-vm/agentloop"
	"github.com/mind-vm/agentloop/llm"
)

// A reply's thought signatures go back with it on every later call: in
// the same run, after the reply's body has been elided, and in the next
// Run, rehydrated from the step store. Gemini 3 stops answering after a
// few turns without them (codeloop findings, 2026-10-02).
func TestThoughtSignaturesFollowTheirReply(t *testing.T) {
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: "```javascript\nconsole.log('one');\n```", ThoughtSignatures: []string{"s1"}},
		llm.CompletionResponse{Content: "```javascript\nconsole.log('two');\n```", ThoughtSignatures: []string{"s2"}},
		llm.CompletionResponse{Content: "DONE\nfinished", ThoughtSignatures: []string{"s3"}},
		llm.CompletionResponse{Content: "DONE\nagain"},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	loop := agentloop.New(agentloop.Config{
		LLM: fake, Sessions: sessions, Steps: newMemorySteps(), SandboxBuilder: emptyBuilder{},
	})
	ctx := context.Background()
	if _, err := loop.Run(ctx, agentloop.RunRequest{SessionID: "s1", Message: "first"}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := loop.Run(ctx, agentloop.RunRequest{SessionID: "s1", Message: "second"}); err != nil {
		t.Fatalf("second run: %v", err)
	}

	calls := fake.Calls()
	if len(calls) != 4 {
		t.Fatalf("got %d calls, want 4", len(calls))
	}
	want := [][]string{
		nil,                // the first call has no reply to carry back
		{"s1"},             // the second carries the first script's
		{"s1", "s2"},       // the third both, the first one's body elided
		{"s1", "s2", "s3"}, // the next Run rehydrates all three from the store
	}
	// The third call sends the first script elided, and its signature
	// with it: that is the case the replay measured.
	var first llm.Message
	for _, m := range calls[2].Messages {
		if m.Role == "assistant" {
			first = m
			break
		}
	}
	if !strings.Contains(first.Content, "body omitted") || !slices.Equal(first.ThoughtSignatures, []string{"s1"}) {
		t.Errorf("third call's first reply = %+v, want the elided body with its signature", first)
	}
	for i, call := range calls {
		var got []string
		for _, m := range call.Messages {
			if m.Role == "assistant" {
				got = append(got, m.ThoughtSignatures...)
			}
		}
		if !slices.Equal(got, want[i]) {
			t.Errorf("call %d carried signatures %v, want %v", i+1, got, want[i])
		}
	}
}
