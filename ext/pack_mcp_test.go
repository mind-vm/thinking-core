package ext_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/mind-vm/thinking-core/ext"
	"github.com/mind-vm/thinking-core/sandbox"
)

// fakeMCP is an MCPSession over a map of canned results.
type fakeMCP struct {
	tools   []ext.MCPTool
	results map[string]ext.MCPResult
	mu      sync.Mutex
	calls   []string
	args    map[string]map[string]any
	err     error
}

func (f *fakeMCP) ListTools(context.Context) ([]ext.MCPTool, error) { return f.tools, f.err }

func (f *fakeMCP) CallTool(_ context.Context, name string, args map[string]any) (ext.MCPResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
	if f.args == nil {
		f.args = map[string]map[string]any{}
	}
	f.args[name] = args
	if f.err != nil {
		return ext.MCPResult{}, f.err
	}
	return f.results[name], nil
}

func newFake() *fakeMCP {
	return &fakeMCP{
		tools: []ext.MCPTool{
			{
				Name:        "get_file-contents",
				Description: "Read a file from a repository.\nSecond line is not shown.",
				ReadOnly:    true,
				InputSchema: map[string]any{
					"type":     "object",
					"required": []any{"path"},
					"properties": map[string]any{
						"path":   map[string]any{"type": "string", "description": "File path"},
						"ref":    map[string]any{"type": []any{"string", "null"}},
						"limit":  map[string]any{"type": "integer"},
						"format": map[string]any{"enum": []any{"raw", "base64"}},
						"tags":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				},
			},
			{Name: "create_issue", Description: "Open an issue", InputSchema: map[string]any{
				"type": "object", "required": []any{"title"},
				"properties": map[string]any{"title": map[string]any{"type": "string"}},
			}},
			{Name: "ping", Description: "Check the server", ReadOnly: true},
		},
		results: map[string]ext.MCPResult{
			"get_file-contents": {Text: "hello"},
			"create_issue":      {Text: `{"number": 7, "url": "https://x/7"}`},
			"ping":              {Text: "007"},
		},
	}
}

type mcpApprover struct {
	answer bool
	err    error
	asked  []string
}

func (r *mcpApprover) Request(d sandbox.InputRequestData) (any, error) {
	r.asked = append(r.asked, d.Message)
	return r.answer, r.err
}

func mcpSandbox(t *testing.T, f *fakeMCP, cfg ext.MCPConfig, policy sandbox.PolicyChecker) (*sandbox.Sandbox, sandbox.Pack) {
	t.Helper()
	ctx := context.Background()
	pack, err := ext.MCPPack(ctx, "gh", f, cfg)
	if err != nil {
		t.Fatalf("MCPPack: %v", err)
	}
	s := sandbox.New(sandbox.RequirePack(ctx), sandbox.SkillDiscoveryPack(nil), pack)
	if policy != nil {
		s.SetPolicy(policy)
	}
	return s, pack
}

func TestMCPPack_PromptDeclaresEveryTool(t *testing.T) {
	_, pack := mcpSandbox(t, newFake(), ext.MCPConfig{}, nil)
	if pack.Name != "mcp/gh" {
		t.Errorf("Name = %q", pack.Name)
	}
	for _, want := range []string{
		`declare module "mcp/gh"`,
		`/** Read a file from a repository. */`,
		// Required path, optional rest, sorted; a type union; an enum; an array.
		`function getFileContents(args: { limit?: number; /** File path */ path: string; ref?: string | null; format?: "raw" | "base64"; tags?: string[]; }): any;`,
		`/** Open an issue (changes something; may ask the user) */`,
		`function createIssue(args: { title: string; }): any;`,
		`function ping(args?: {}): any;`,
	} {
		// Field order is sorted by name, so build the expectation loosely
		// for the one multi-field declaration.
		if strings.Contains(want, "getFileContents") {
			for _, part := range []string{"function getFileContents(args: {", "/** File path */ path: string;", "ref?: string | null;", `format?: "raw" | "base64";`, "tags?: string[];", "limit?: number;"} {
				if !strings.Contains(pack.Prompt, part) {
					t.Errorf("Prompt missing %q:\n%s", part, pack.Prompt)
				}
			}
			continue
		}
		if !strings.Contains(pack.Prompt, want) {
			t.Errorf("Prompt missing %q:\n%s", want, pack.Prompt)
		}
	}
	if strings.Contains(pack.Prompt, "Second line") {
		t.Errorf("only the description's first line belongs in the prompt:\n%s", pack.Prompt)
	}
}

func TestMCPPack_ManyToolsBecomeACatalog(t *testing.T) {
	f := &fakeMCP{results: map[string]ext.MCPResult{}}
	for i := 0; i < 60; i++ {
		f.tools = append(f.tools, ext.MCPTool{Name: fmt.Sprintf("tool_%d", i), Description: "does thing", ReadOnly: true})
	}
	_, pack := mcpSandbox(t, f, ext.MCPConfig{}, nil)
	if strings.Contains(pack.Prompt, "declare module") || strings.Contains(pack.Prompt, "function tool0(") {
		t.Errorf("a large server's declarations should stay out of the prompt:\n%s", pack.Prompt)
	}
	for _, want := range []string{`skillGet("mcp/gh")`, "tool0 — does thing", "and 20 more"} {
		if !strings.Contains(pack.Prompt, want) {
			t.Errorf("Prompt missing %q:\n%s", want, pack.Prompt)
		}
	}
	if !strings.Contains(pack.HelpEntries["mcp/gh"], "function tool59(") {
		t.Errorf("full declarations belong behind skillGet")
	}
}

func TestMCPPack_ScriptCanSeeEveryToolAndSkillGet(t *testing.T) {
	f := &fakeMCP{results: map[string]ext.MCPResult{}}
	for i := 0; i < 20; i++ {
		f.tools = append(f.tools, ext.MCPTool{Name: fmt.Sprintf("tool_%d", i), ReadOnly: true})
	}
	s, _ := mcpSandbox(t, f, ext.MCPConfig{}, nil)
	out, err := s.Execute(`skillGet("mcp/gh").indexOf("function tool19(") > 0`)
	if err != nil || out != "true" {
		t.Fatalf("skillGet: %q, %v", out, err)
	}
}

func TestMCPPack_CamelCaseAndDedupe(t *testing.T) {
	f := &fakeMCP{
		tools: []ext.MCPTool{
			{Name: "get_file", ReadOnly: true}, {Name: "get-file", ReadOnly: true},
			{Name: "SearchRepos", ReadOnly: true}, {Name: "delete", ReadOnly: true}, {Name: "1st", ReadOnly: true},
		},
		results: map[string]ext.MCPResult{"get-file": {Text: "second"}},
	}
	s, _ := mcpSandbox(t, f, ext.MCPConfig{}, nil)
	out, err := s.Execute(`Object.keys(require("mcp/gh")).join(",")`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "getFile,getFile2,searchRepos,deleteTool,_1st"; out != want {
		t.Errorf("keys = %q, want %q", out, want)
	}
	// The second getFile must reach the server under its own name.
	if out, err = s.Execute(`require("mcp/gh").getFile2({})`); err != nil || out != "second" {
		t.Errorf("getFile2 = %q, %v", out, err)
	}
}

func TestMCPPack_ResultShapes(t *testing.T) {
	f := newFake()
	f.results["get_file-contents"] = ext.MCPResult{Text: "ignored", Structured: map[string]any{"lines": []any{"a", "b"}}}
	s, _ := mcpSandbox(t, f, ext.MCPConfig{}, sandbox.AllowAll)

	cases := []struct{ code, want string }{
		{`JSON.stringify(require("mcp/gh").getFileContents({path:"a"}))`, `{"lines":["a","b"]}`},
		{`(function(){ var r = require("mcp/gh").createIssue({title:"t"}); return r.number + "|" + r.url; })()`, `7|https://x/7`},
		// Scalar text stays a string: "007" must not become 7.
		{`typeof require("mcp/gh").ping() + ":" + require("mcp/gh").ping()`, `string:007`},
	}
	for _, c := range cases {
		out, err := s.Execute(c.code)
		if err != nil || out != c.want {
			t.Errorf("%s\n got %q, %v\nwant %q", c.code, out, err, c.want)
		}
	}
	if got := f.args["get_file-contents"]["path"]; got != "a" {
		t.Errorf("args did not reach the server: %v", f.args)
	}
}

func TestMCPPack_IsErrorThrows(t *testing.T) {
	f := newFake()
	f.results["ping"] = ext.MCPResult{IsError: true, Text: "rate limited, retry in 60s"}
	s, _ := mcpSandbox(t, f, ext.MCPConfig{}, nil)
	out, err := s.Execute(`
		var r;
		try { require("mcp/gh").ping(); } catch (e) { r = (e instanceof Error) + ":" + e.message; }
		r`)
	if err != nil || out != "true:rate limited, retry in 60s" {
		t.Errorf("got %q, %v", out, err)
	}
}

func TestMCPPack_TransportErrorNamesTheTool(t *testing.T) {
	f := newFake()
	s, _ := mcpSandbox(t, f, ext.MCPConfig{}, nil)
	f.err = errors.New("connection closed")
	_, err := s.Execute(`require("mcp/gh").ping()`)
	if err == nil || !strings.Contains(err.Error(), "mcp.gh.ping") || !strings.Contains(err.Error(), "connection closed") {
		t.Errorf("err = %v", err)
	}
}

func TestMCPPack_BadArgument(t *testing.T) {
	s, _ := mcpSandbox(t, newFake(), ext.MCPConfig{}, nil)
	_, err := s.Execute(`require("mcp/gh").ping("x")`)
	if err == nil || !strings.Contains(err.Error(), "one object") {
		t.Errorf("err = %v", err)
	}
}

// --- policy ---------------------------------------------------------

func TestMCPPack_ReadOnlyIsAllowedWriteIsDenied(t *testing.T) {
	f := newFake()
	s, _ := mcpSandbox(t, f, ext.MCPConfig{}, sandbox.DefaultPolicy{})

	if _, err := s.Execute(`require("mcp/gh").ping()`); err != nil {
		t.Errorf("a read-only tool should pass DefaultPolicy: %v", err)
	}
	_, err := s.Execute(`require("mcp/gh").createIssue({title:"t"})`)
	if err == nil || !strings.Contains(err.Error(), "mcp.gh.create_issue") || !strings.Contains(err.Error(), "not permitted") {
		t.Errorf("a write should be denied, err = %v", err)
	}
	if strings.Contains(strings.Join(f.calls, ","), "create_issue") {
		t.Errorf("a denied call reached the server: %v", f.calls)
	}
}

func TestMCPPack_IgnoreReadOnlyHint(t *testing.T) {
	s, _ := mcpSandbox(t, newFake(), ext.MCPConfig{IgnoreReadOnlyHint: true}, sandbox.DefaultPolicy{})
	if _, err := s.Execute(`require("mcp/gh").ping()`); err == nil {
		t.Error("with the hint ignored, ping is a side effect and should be denied")
	}
}

func TestMCPPack_Grants(t *testing.T) {
	for name, grants := range map[string][]string{
		"exact":  {"mcp.gh.create_issue"},
		"server": {"mcp.gh.*"},
	} {
		t.Run(name, func(t *testing.T) {
			s, _ := mcpSandbox(t, newFake(), ext.MCPConfig{}, sandbox.DefaultPolicy{AllowTools: grants})
			if _, err := s.Execute(`require("mcp/gh").createIssue({title:"t"})`); err != nil {
				t.Errorf("granted by %v but denied: %v", grants, err)
			}
		})
	}
	// A grant for another server, or the wrong tool, must not leak.
	for _, grants := range [][]string{{"mcp.other.*"}, {"mcp.gh.other_tool"}, {"mcp.*"}, {"*"}} {
		s, _ := mcpSandbox(t, newFake(), ext.MCPConfig{}, sandbox.DefaultPolicy{AllowTools: grants})
		if _, err := s.Execute(`require("mcp/gh").createIssue({title:"t"})`); err == nil {
			t.Errorf("%v should not grant mcp.gh.create_issue", grants)
		}
	}
}

func TestMCPPack_AsksAndRemembers(t *testing.T) {
	f := newFake()
	ap := &mcpApprover{answer: true}
	s, _ := mcpSandbox(t, f, ext.MCPConfig{Approver: ap}, sandbox.DefaultPolicy{})

	for i := 0; i < 3; i++ {
		if _, err := s.Execute(`require("mcp/gh").createIssue({title:"t"})`); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if len(ap.asked) != 1 {
		t.Fatalf("asked %d times, want once: %v", len(ap.asked), ap.asked)
	}
	if !strings.Contains(ap.asked[0], "create_issue") || !strings.Contains(ap.asked[0], "Open an issue") {
		t.Errorf("question = %q", ap.asked[0])
	}
	if len(f.calls) != 3 {
		t.Errorf("calls = %v", f.calls)
	}
}

func TestMCPPack_DeclinedIsNotRemembered(t *testing.T) {
	f := newFake()
	ap := &mcpApprover{answer: false}
	s, _ := mcpSandbox(t, f, ext.MCPConfig{Approver: ap}, sandbox.DefaultPolicy{})
	for i := 0; i < 2; i++ {
		_, err := s.Execute(`require("mcp/gh").createIssue({title:"t"})`)
		if err == nil || !strings.Contains(err.Error(), "declined") {
			t.Fatalf("err = %v", err)
		}
	}
	if len(ap.asked) != 2 || len(f.calls) != 0 {
		t.Errorf("asked=%d calls=%v", len(ap.asked), f.calls)
	}
}

func TestMCPPack_ReadOnlyNeverAsks(t *testing.T) {
	ap := &mcpApprover{answer: true}
	s, _ := mcpSandbox(t, newFake(), ext.MCPConfig{Approver: ap}, sandbox.DefaultPolicy{})
	if _, err := s.Execute(`require("mcp/gh").ping()`); err != nil || len(ap.asked) != 0 {
		t.Errorf("err=%v asked=%v", err, ap.asked)
	}
}

func TestMCPPack_PolicySeesArguments(t *testing.T) {
	var seen map[string]any
	var name string
	pol := policyFunc(func(tool string, args map[string]any) (sandbox.PolicyCheckResult, error) {
		if strings.HasPrefix(tool, "mcp.") {
			name, seen = tool, args
		}
		return sandbox.PolicyCheckResult{Allowed: true}, nil
	})
	s, _ := mcpSandbox(t, newFake(), ext.MCPConfig{}, pol)
	if _, err := s.Execute(`require("mcp/gh").createIssue({title:"hi"})`); err != nil {
		t.Fatal(err)
	}
	inner, _ := seen["arguments"].(map[string]any)
	if name != "mcp.gh.create_issue" || seen["readOnly"] != false || seen["server"] != "gh" || inner["title"] != "hi" {
		t.Errorf("name=%q args=%v", name, seen)
	}
}

type policyFunc func(string, map[string]any) (sandbox.PolicyCheckResult, error)

func (p policyFunc) Check(_ context.Context, tool string, args map[string]any) (sandbox.PolicyCheckResult, error) {
	return p(tool, args)
}

// --- construction ---------------------------------------------------

func TestMCPPack_ConstructionErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := ext.MCPPack(ctx, "bad.name", newFake(), ext.MCPConfig{}); err == nil {
		t.Error("a dot in the server name makes mcp.<server>.* ambiguous")
	}
	if _, err := ext.MCPPack(ctx, "", newFake(), ext.MCPConfig{}); err == nil {
		t.Error("empty name")
	}
	if _, err := ext.MCPPack(ctx, "x", &fakeMCP{}, ext.MCPConfig{}); err == nil {
		t.Error("a server with no tools is a config mistake")
	}
	if _, err := ext.MCPPack(ctx, "x", &fakeMCP{err: errors.New("boom")}, ext.MCPConfig{}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("list error not surfaced: %v", err)
	}
	if _, err := ext.MCPPack(ctx, "x", nil, ext.MCPConfig{}); err == nil {
		t.Error("nil session")
	}
}
