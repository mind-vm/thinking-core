package agentloop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/thinking-core/agentloop"
	"github.com/mind-vm/thinking-core/llm"
)

func loopWith(cfg agentloop.Config, codes ...string) (agentloop.Loop, *fakeLLM) {
	resps := make([]llm.CompletionResponse, 0, len(codes))
	for _, c := range codes {
		resps = append(resps, script(c))
	}
	fake := newFakeLLM("fake", resps...)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	cfg.LLM, cfg.Sessions, cfg.Steps, cfg.SandboxBuilder = fake, sessions, newMemorySteps(), dataBuilder{}
	return agentloop.New(cfg), fake
}

// A cancelled Run must end with the context's error, not feed the interrupt
// back to the model as a script bug to fix.
func TestRun_CancelInterruptsARunningScript(t *testing.T) {
	loop, fake := loopWith(agentloop.Config{}, `while (true) {}`, `answer("never")`)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	start := time.Now()
	_, err := loop.Run(ctx, agentloop.RunRequest{SessionID: "s1", Message: "go"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Run took %s to stop", elapsed)
	}
	if n := len(fake.Calls()); n != 1 {
		t.Errorf("LLM calls = %d; the interrupt must not be fed back to the model", n)
	}
}

func TestRun_StateOverTheLimitKeepsPreviousState(t *testing.T) {
	loop, fake := loopWith(agentloop.Config{MaxStateBytes: 200},
		`state.keep = 1`,
		`state.big = "x".repeat(1000)`,
		`answer(JSON.stringify(state))`,
	)
	res, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "go"})
	if err != nil {
		t.Fatal(err)
	}
	calls := fake.Calls()
	if len(calls) != 3 {
		t.Fatalf("calls = %d", len(calls))
	}
	msgs := calls[2].Messages
	var last string
	for _, m := range msgs[len(msgs)-3:] {
		last += m.Content + "\n"
	}
	if !strings.Contains(last, "200 byte limit") || !strings.Contains(last, "keep less") {
		t.Errorf("the model should be told the limit and what to do:\n%s", last)
	}
	if !strings.Contains(res.FinalText, `"keep":1`) || strings.Contains(res.FinalText, "big") {
		t.Errorf("final state = %q, want the state from before the refused turn", res.FinalText)
	}
}
