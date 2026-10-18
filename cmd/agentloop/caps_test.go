package main

import (
	"context"
	"strings"
	"testing"

	"github.com/mind-vm/thinking-core/agentloop"
	"github.com/mind-vm/thinking-core/sandbox"
)

// askSandbox builds the CLI's capability slice around p and returns a
// sandbox holding only the ask capability's packs.
func askSandbox(t *testing.T, p *terminalPrompter) *sandbox.Sandbox {
	t.Helper()
	caps := buildCapabilities(&options{}, nil, p, p.questions(), nil, nil)
	for _, c := range caps {
		if c.Name != "ask" {
			continue
		}
		packs, err := c.Build(agentloop.BuildContext{Ctx: context.Background()})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		return sandbox.New(packs...)
	}
	t.Fatal("the CLI's capabilities have no ask")
	return nil
}

// A question reaches the terminal and its answer reaches the script.
func TestAskReachesTheTerminal(t *testing.T) {
	p, out := prompterFor(t, approvalPrompt, "y\n2\nAda\n", nil)
	sb := askSandbox(t, p)

	got, err := sb.Execute(`ask({type: "confirm", message: "Go ahead?"})`)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if got != "true" {
		t.Errorf("confirm answered %q, want true", got)
	}
	if !strings.Contains(out.String(), "Go ahead?") {
		t.Errorf("the question never reached the terminal: %q", out.String())
	}

	got, err = sb.Execute(`ask({type: "choice", message: "Which?", options: ["red", "blue"]})`)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if got != "blue" {
		t.Errorf("choice answered %q, want blue", got)
	}

	got, err = sb.Execute(`ask({type: "text", message: "Name?"})`)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if got != "Ada" {
		t.Errorf("text answered %q, want Ada", got)
	}
}

// A question is not a permission: a yes is not remembered, so the same
// question asked again is asked again, and it leaves no grant behind
// for `sessions show` to list.
func TestAskIsNotRemembered(t *testing.T) {
	p, _ := prompterFor(t, approvalPrompt, "y\nn\n", nil)
	sb := askSandbox(t, p)

	first, err := sb.Execute(`ask({type: "confirm", message: "Delete the drafts?"})`)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	second, err := sb.Execute(`ask({type: "confirm", message: "Delete the drafts?"})`)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if first != "true" || second != "false" {
		t.Fatalf("answers %q then %q; the second was answered from memory", first, second)
	}
	if _, known := p.recall("Delete the drafts?"); known {
		t.Error("the question was recorded as a grant")
	}
}

// --approve auto and deny answer "may I?", not questions. Under either,
// ask() fails so the agent plans around the silence, instead of being
// told yes (or no) by a flag that was never about its question.
func TestAskHasNoAnswerWithoutSomeoneToAsk(t *testing.T) {
	for _, mode := range []approvalMode{approvalAuto, approvalDeny} {
		t.Run(string(mode), func(t *testing.T) {
			p, out := prompterFor(t, mode, "y\n", nil)
			sb := askSandbox(t, p)
			_, err := sb.Execute(`ask({type: "confirm", message: "Go ahead?"})`)
			if err == nil || !strings.Contains(err.Error(), errNoTerminal.Error()) {
				t.Fatalf("got %v, want %v", err, errNoTerminal)
			}
			if out.Len() != 0 {
				t.Errorf("something was printed with nobody to read it: %q", out.String())
			}
		})
	}
}
