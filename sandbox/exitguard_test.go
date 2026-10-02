package sandbox

import (
	"errors"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// effectPack emits a file write when its write() is called, as the
// workspace pack does.
func effectPack() Pack {
	return Pack{Name: "effect", Register: func(rt *goja.Runtime, sb *Sandbox) {
		_ = rt.Set("write", func() { sb.Emit(Event{Kind: EventFileWrite, Summary: "writeFile: a.go"}) })
	}}
}

func TestExitGuardRefusesAndTurnEffectsReset(t *testing.T) {
	s := New(effectPack())
	s.EnableUserOutput()
	s.SetExitGuard(func() error {
		if fx := s.TurnEffects(); len(fx) > 0 {
			return errors.New("exit: not in the script that did " + strings.Join(fx, ", "))
		}
		return nil
	})

	_, _, err := s.ExecuteTurn(`write(); exit("done")`, nil)
	if err == nil || !strings.Contains(err.Error(), "writeFile: a.go") {
		t.Fatalf("exit after a write should be refused naming it, got %v", err)
	}
	if _, called := s.TakeDone(); called {
		t.Fatal("a refused exit must not finish the task")
	}

	turn(t, s, `exit("done")`)
	if out, called := s.TakeDone(); !called || out != "done" {
		t.Errorf("the next script's exit should finish: %q %v", out, called)
	}
}

func TestNoGuardLeavesExitAlone(t *testing.T) {
	s := New(effectPack())
	s.EnableUserOutput()
	turn(t, s, `write(); exit("done")`)
	if _, called := s.TakeDone(); !called {
		t.Error("without a guard exit finishes as before")
	}
}
