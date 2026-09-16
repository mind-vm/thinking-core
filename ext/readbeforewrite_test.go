package ext_test

import (
	"strings"
	"testing"

	"github.com/mind-vm/agentloop/ext"
)

// Under RequireReadBeforeWrite an edit of a file the run has not read is
// refused and told how to proceed. Reading it first, naming it in
// PreRead, or creating a new file all pass.
func TestRequireReadBeforeWrite(t *testing.T) {
	files := map[string]string{"a.go": "package x\n", "b.go": "package x\n"}

	sb, _ := workspaceAt(t, files, ext.WorkspaceOptions{RequireReadBeforeWrite: true, PreRead: []string{"b.go"}}, nil)

	if _, _, err := sb.ExecuteTurn(`editFile("a.go", "package x", "package y");`, nil); err == nil {
		t.Fatal("editing an unread file should be refused")
	} else if !strings.Contains(err.Error(), "readFile") {
		t.Errorf("refusal should name the way out, got %v", err)
	}

	if _, _, err := sb.ExecuteTurn(`readFile("a.go"); editFile("a.go", "package x", "package y");`, nil); err != nil {
		t.Errorf("reading first should allow the edit: %v", err)
	}
	if _, _, err := sb.ExecuteTurn(`editFile("b.go", "package x", "package y");`, nil); err != nil {
		t.Errorf("a preloaded file counts as read: %v", err)
	}
	if _, _, err := sb.ExecuteTurn(`writeFile("new.go", "package x\n");`, nil); err != nil {
		t.Errorf("a new file has nothing to read: %v", err)
	}
}
