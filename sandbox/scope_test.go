package sandbox

import (
	"strings"
	"testing"
)

// Each script runs in its own closure: its declarations do not reach the
// next script, and what it means to carry forward it puts in state.
// A REPL scope was measured in labs/codeloop and moved nothing, so
// there is one scope, and this holds it.
func TestScriptsDoNotShareDeclarations(t *testing.T) {
	s := New()
	if _, _, err := s.ExecuteTurn("const carried = 41;", nil); err != nil {
		t.Fatalf("first script: %v", err)
	}
	if logs, _, err := s.ExecuteTurn("log(carried);", nil); err == nil {
		t.Fatalf("second script saw %q, want a ReferenceError", logs)
	}
	_, ret, err := s.ExecuteTurn("state.v = 42;", nil)
	if err != nil || !strings.Contains(string(ret), "42") {
		t.Fatalf("state: %v %s", err, ret)
	}
}
