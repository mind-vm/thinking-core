package ext

import (
	"context"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/dop251/goja"

	"github.com/mind-vm/thinking-core/sandbox"
)

// WorkspaceOptions tunes WorkspacePack. The zero value is usable: every
// field falls back to a documented default.
type WorkspaceOptions struct {
	// MaxFileBytes caps a single readFile and skips larger files during
	// grep. Zero uses 1 MiB. The cap exists because the loop threads a
	// script's return value forward in full — an accidental read of a
	// huge file is a context-window problem, not just a slow one.
	MaxFileBytes int64

	// MaxResults caps how many entries glob, grep, and listDir return.
	// Zero uses 500.
	MaxResults int

	// MaxWalkFiles bounds how many entries a single glob or grep visits
	// before giving up, so a pattern pointed at an enormous tree fails
	// loudly instead of hanging the run. Zero uses 50,000.
	MaxWalkFiles int

	// RequireReadBeforeWrite refuses a writeFile or editFile on a path
	// the run has not read. An edit composed from memory is how
	// "does not contain that text" happens; reading first is what a
	// person does and what the refusal asks for.
	RequireReadBeforeWrite bool

	// PreRead are paths the run may write without reading, because
	// their contents were already put in front of the model — a
	// preloaded workspace has been read in every sense that matters.
	PreRead []string

	// SkipDirs are directory names never descended into. Nil uses
	// {".git"} — not source, frequently enormous, and its config can
	// hold credentials. Set explicitly to add vendor trees.
	SkipDirs []string

	// Approver is asked to allow a mutation the policy denied. Nil means
	// no human is available, and a denied write simply fails — the same
	// contract the fetch pack's InputRequester has.
	Approver sandbox.InputRequester

	// Refuse, when it returns a non-empty reason, refuses a writeFile or
	// editFile on that path outright: before the policy, and without
	// asking the Approver. It is for files no one should change by hand
	// — a lockfile, generated code — where a yes in the approval prompt
	// would still be the wrong change. The reason reaches the model
	// verbatim, so it should name what to do instead.
	Refuse func(rel string) string

	// BeforeWrite runs once a mutation is authorised and before anything
	// is written, with the path about to change. It is where a caller
	// records the state the change will be compared against.
	BeforeWrite func(ctx context.Context, rel string)
}

func (o WorkspaceOptions) maxFileBytes() int64 {
	if o.MaxFileBytes <= 0 {
		return 1 << 20
	}
	return o.MaxFileBytes
}

func (o WorkspaceOptions) maxResults() int {
	if o.MaxResults <= 0 {
		return 500
	}
	return o.MaxResults
}

func (o WorkspaceOptions) maxWalkFiles() int {
	if o.MaxWalkFiles <= 0 {
		return 50_000
	}
	return o.MaxWalkFiles
}

func (o WorkspaceOptions) skipDirs() []string {
	if o.SkipDirs == nil {
		return []string{".git"}
	}
	return o.SkipDirs
}

// WorkspacePack exposes a project directory to the agent: readFile,
// writeFile, editFile, listDir, glob, and grep.
//
// Every path is resolved by root, an *os.Root, so ".." and symlinks
// cannot escape the directory the caller opened — the confinement is
// enforced by the runtime rather than by path arithmetic here, which is
// where this kind of thing is usually got wrong.
//
// Reads are ungated. Mutations are policy-gated as "writeFile" and
// "editFile"; when the policy denies one and an Approver is configured,
// the user is asked to allow it, and both primitives ask the same
// question about a given path so approving an edit does not then
// prompt again for a write to the same file.
func WorkspacePack(ctx context.Context, root *os.Root, opts WorkspaceOptions) sandbox.Pack {
	w := &workspace{ctx: ctx, root: root, opts: opts}
	for _, p := range opts.PreRead {
		w.markRead(p)
	}

	return sandbox.Pack{
		Name:        "workspace",
		Description: "Read and change files in the project — readFile, writeFile, editFile, listDir, glob, grep",
		Prompt: `// --- Workspace ---
// Paths are relative to the project root. ".." and absolute paths are refused.
/** Read a text file. offset is a 1-based LINE number, limit a line count. */
declare function readFile(path: string, opts?: { offset?: number; limit?: number }): string;
/** Create or replace a file, making parent directories as needed. Returns true. */
declare function writeFile(path: string, content: string): boolean;
/** Replace find with replace in a file. find must appear EXACTLY once unless {all:true}. Returns the number of replacements. */
declare function editFile(path: string, find: string, replace: string, opts?: { all?: boolean }): number;
/** List one directory (default the project root). */
declare function listDir(path?: string): Array<{ name: string; path: string; dir: boolean; size: number }>;
/** Find files by glob. ** matches across directories, e.g. "src/**\/*.go". */
declare function glob(pattern: string): string[];
/** Search file contents by regular expression (Go syntax, RE2). */
declare function grep(pattern: string, opts?: { glob?: string; limit?: number; ignoreCase?: boolean }): Array<{ path: string; line: number; text: string }>;`,
		HelpEntries: map[string]string{
			"readFile": `readFile(path, opts?) — Read a text file, relative to the project root.
  opts.offset is a 1-based LINE number; opts.limit is a count of lines.
  A file over the size cap comes back truncated, with a marker saying so.
  Example: readFile("src/main.go", { offset: 40, limit: 20 });`,
			"writeFile": `writeFile(path, content) — Create or replace a file. Parent directories are created.
  Needs permission the first time it touches a given path.
  Example: writeFile("docs/notes.md", "# Notes\n");`,
			"editFile": `editFile(path, find, replace, opts?) — Replace an exact substring in a file.
  find must occur EXACTLY once, or it throws and nothing is written — pass
  {all: true} to replace every occurrence. Returns how many were replaced.
  Prefer this over writeFile for a change to part of a file.
  Example: editFile("src/main.go", "oldName", "newName");`,
			"listDir": `listDir(path?) — List one directory, default the project root.
  Returns [{name, path, dir, size}]. Not recursive; use glob for that.
  Example: listDir("src");`,
			"glob": `glob(pattern) — Find files by pattern, relative to the project root.
  ** matches across directories, * within one segment.
  Example: glob("**/*_test.go");`,
			"grep": `grep(pattern, opts?) — Search file contents with a Go (RE2) regular expression.
  opts.glob narrows which files are searched; opts.ignoreCase folds case;
  opts.limit caps matches. Binary and oversized files are skipped.
  Example: grep("func Test\\w+", { glob: "**/*.go", limit: 50 });`,
		},
		Register: func(rt *goja.Runtime, sb *sandbox.Sandbox) {
			w.rt, w.sb = rt, sb
			_ = rt.Set("readFile", w.readFile)
			_ = rt.Set("writeFile", w.writeFile)
			_ = rt.Set("editFile", w.editFile)
			_ = rt.Set("listDir", w.listDir)
			_ = rt.Set("glob", w.globFiles)
			_ = rt.Set("grep", w.grep)
		},
	}
}

type workspace struct {
	ctx  context.Context
	root *os.Root
	opts WorkspaceOptions
	rt   *goja.Runtime
	sb   *sandbox.Sandbox

	// read is every path readFile has returned this run, plus whatever
	// PreRead named. It gates mutations under RequireReadBeforeWrite.
	read map[string]bool
}

// markRead records a path as read, so a later write may proceed.
func (w *workspace) markRead(rel string) {
	if w.read == nil {
		w.read = map[string]bool{}
	}
	w.read[rel] = true
}

// requireRead refuses a mutation of a file the run has not looked at.
// The refusal names the file and the one call that unblocks it: in this
// loop the error string is the only documentation the model re-reads.
func (w *workspace) requireRead(tool, rel string) {
	if !w.opts.RequireReadBeforeWrite || w.read[rel] {
		return
	}
	if _, err := w.root.Stat(rel); err != nil {
		return // a new file has nothing to read
	}
	w.throw("%s: %s has not been read this run — call readFile(%q) first, then edit what it actually contains", tool, rel, rel)
}

// --- path handling --------------------------------------------------

// rel normalises a path from the script into one relative to the root.
//
// os.Root would refuse an escape anyway; rejecting absolute paths and
// obvious traversal here just makes the refusal say something useful
// instead of surfacing a syscall error. Note that "/etc/passwd" is
// refused rather than quietly reinterpreted as "etc/passwd" under the
// root — silently answering a different question than the one asked is
// how a caller comes to believe it read something it did not.
func (w *workspace) rel(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return ".", nil
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return "", fmt.Errorf("path %q is absolute; paths are relative to the project root", p)
	}
	clean := path.Clean(filepathToSlash(p))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path %q leaves the project root", p)
	}
	return clean, nil
}

func filepathToSlash(p string) string { return strings.ReplaceAll(p, `\`, "/") }

func (w *workspace) throw(format string, args ...any) {
	panic(w.rt.NewGoError(fmt.Errorf(format, args...)))
}

// --- authorisation --------------------------------------------------

// authorize gates a mutation. The policy is consulted first, so a
// deployment that grants writes outright never prompts; a denial then
// becomes a question for the user, when there is someone to ask.
//
// Both mutating primitives ask the same question about a given path.
// The approval is remembered by whatever answers it, so editing a file
// and then writing it troubles the user once rather than twice.
func (w *workspace) authorize(tool, rel string) {
	res, err := w.sb.CheckPolicy(w.ctx, tool, map[string]any{"path": rel})
	if err != nil {
		w.throw("%s: policy check failed: %v", tool, err)
	}
	if res.Allowed {
		return
	}

	reason := res.Reason
	if reason == "" {
		reason = "denied by policy"
	}
	if w.opts.Approver == nil {
		w.sb.Emit(sandbox.Event{Kind: sandbox.EventError, Summary: tool + " blocked by policy", Detail: reason})
		w.throw("%s: %s", tool, reason)
	}

	question := fmt.Sprintf("Allow the agent to modify %s?", rel)
	w.sb.Emit(sandbox.Event{Kind: sandbox.EventAsk, Summary: "requesting permission to modify " + rel})
	value, err := w.opts.Approver.Request(sandbox.InputRequestData{InputType: "confirm", Message: question})
	if err != nil {
		w.sb.Emit(sandbox.Event{Kind: sandbox.EventError, Summary: "permission request failed", Detail: err.Error()})
		w.throw("%s: %s (could not ask: %v)", tool, reason, err)
	}
	if approved, ok := value.(bool); !ok || !approved {
		w.sb.Emit(sandbox.Event{Kind: sandbox.EventAskResult, Summary: "denied: " + rel})
		w.throw("%s: the user declined to allow changes to %s", tool, rel)
	}
	w.sb.Emit(sandbox.Event{Kind: sandbox.EventAskResult, Summary: "approved: " + rel})
}

// refuse applies WorkspaceOptions.Refuse. It runs before requireRead
// and authorize: reading a file first does not make it editable, and
// nobody should be asked to approve a change that is wrong either way.
func (w *workspace) refuse(tool, rel string) {
	if w.opts.Refuse == nil {
		return
	}
	if reason := w.opts.Refuse(rel); reason != "" {
		w.sb.Emit(sandbox.Event{Kind: sandbox.EventError, Summary: tool + " refused: " + rel, Detail: reason})
		w.throw("%s: %s", tool, reason)
	}
}

// beforeWrite applies WorkspaceOptions.BeforeWrite.
func (w *workspace) beforeWrite(rel string) {
	if w.opts.BeforeWrite != nil {
		w.opts.BeforeWrite(w.ctx, rel)
	}
}
