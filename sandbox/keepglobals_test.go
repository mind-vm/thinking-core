package sandbox

import (
	"strings"
	"testing"
)

// A closure-scoped script loses its own declarations; a REPL-scoped one
// keeps them. This is the only difference the two modes are meant to have.
func TestKeepGlobalsCarriesDeclarations(t *testing.T) {
	for _, tc := range []struct{ keep, wantErr bool }{{false, true}, {true, false}} {
		s := New()
		s.SetKeepGlobals(tc.keep)
		if _, _, err := s.ExecuteTurn("const carried = 41; let also = 1;", nil); err != nil {
			t.Fatalf("keep=%v first script: %v", tc.keep, err)
		}
		logs, _, err := s.ExecuteTurn("log(carried + also);", nil)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("keep=false: second script saw %q, want a ReferenceError", logs)
			}
			continue
		}
		if err != nil {
			t.Fatalf("keep=true second script: %v", err)
		}
		if !strings.Contains(logs, "42") {
			t.Fatalf("keep=true: logs = %q, want 42", logs)
		}
	}
}

// A REPL-scoped script may still use the top-level return the protocol
// documents: it falls back to the closure rather than failing to parse.
func TestKeepGlobalsAllowsTopLevelReturn(t *testing.T) {
	s := New()
	s.SetKeepGlobals(true)
	if _, _, err := s.ExecuteTurn(`const kept = 7;`, nil); err != nil {
		t.Fatalf("first script: %v", err)
	}
	logs, ret, err := s.ExecuteTurn(`log("threading"); return {v: 42};`, nil)
	if err != nil {
		t.Fatalf("top-level return under keepGlobals: %v (logs %q)", err, logs)
	}
	if !strings.Contains(string(ret), "42") {
		t.Errorf("return not threaded: %s", ret)
	}
	// The earlier declaration still survives for a script that does not return.
	if logs, _, err := s.ExecuteTurn(`log(kept);`, nil); err != nil || !strings.Contains(logs, "7") {
		t.Errorf("persistence lost: %v %q", err, logs)
	}
}
