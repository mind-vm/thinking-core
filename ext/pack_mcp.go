package ext

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/dop251/goja"

	"github.com/mind-vm/thinking-core/sandbox"
)

// MCPTool is one tool an MCP server offers, reduced to what the pack
// needs. It is this package's own type rather than the SDK's so ext
// carries no MCP dependency: the module that does speak the protocol
// (ext/mcp) converts into it.
type MCPTool struct {
	Name        string
	Description string

	// InputSchema is the tool's JSON Schema for its arguments, used only
	// to render the TypeScript declaration the model reads. Nothing is
	// validated client-side; the server rejects bad arguments itself.
	InputSchema map[string]any

	// ReadOnly is the server's annotations.readOnlyHint. It is a hint
	// from the server, not a guarantee — see MCPConfig.IgnoreReadOnlyHint.
	ReadOnly bool
}

// MCPResult is what one tools/call returned, already flattened.
type MCPResult struct {
	// Text is the text content blocks joined with newlines. Non-text
	// blocks (images, audio) are the session's to describe or drop; the
	// sandbox has nowhere to put bytes.
	Text string

	// Structured is the result's structuredContent, nil when absent. It
	// wins over Text because a server that bothered to return both means
	// the structured form to be the data and the text a rendering of it.
	Structured any

	// IsError is the protocol's isError flag: the tool ran and failed.
	// Text then carries the reason, which the model gets as an exception.
	IsError bool
}

// MCPSession is the slice of an MCP client the pack needs. Implementing
// it over the official SDK is ext/mcp's job; a test implements it with a
// map.
type MCPSession interface {
	ListTools(ctx context.Context) ([]MCPTool, error)
	CallTool(ctx context.Context, name string, args map[string]any) (MCPResult, error)
}

// MCPConfig configures MCPPack. The zero value is usable.
type MCPConfig struct {
	// Description is the skillList() one-liner. Defaults to a generic
	// line naming the server.
	Description string

	// Approver is asked to allow a call the policy denied. Nil means no
	// human is available, and a denied call simply throws — the same
	// contract WorkspaceOptions.Approver has.
	Approver sandbox.InputRequester

	// InlineTools is how many tools get their full declaration in the
	// system prompt; above it the prompt carries one catalog line per
	// tool and skillGet("mcp/<server>") returns the declarations. Zero
	// uses 12. A server can offer hundreds of tools, and the prompt is
	// paid for on every turn.
	InlineTools int

	// IgnoreReadOnlyHint treats every tool as a side effect, whatever the
	// server claims. Use it for a server you do not trust: the hint is
	// the server's own word, and a hostile one can mark a destructive
	// tool read-only to skip the permission gate.
	IgnoreReadOnlyHint bool
}

func (c MCPConfig) inlineTools() int {
	if c.InlineTools <= 0 {
		return 12
	}
	return c.InlineTools
}

// maxCatalogLines bounds the catalog in the prompt. Past it the model is
// told how many more exist and pointed at skillGet; a thousand-line
// prompt is the failure the catalog form exists to avoid.
const maxCatalogLines = 40

var mcpServerRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// MCPPack exposes an MCP server's tools as functions inside the sandbox:
// require('mcp/<serverName>') returns one synchronous function per tool,
// camelCased, taking a single arguments object. This is the "code mode"
// pattern ADR-0004 describes — a script chains several tool calls and
// keeps the intermediate results in variables, instead of paying a model
// round-trip (and the tokens of every intermediate result) per call.
//
// Tools are listed once, here, at build time. A server whose tool set
// changes mid-session is not followed; the next sandbox build sees it.
//
// A call returns the result's structured content when the server sent
// any, else its text — parsed when it is a JSON object or array, a string
// otherwise. Scalars are deliberately left as strings: "12345678901234567890"
// or "007" parsed as numbers would be silently wrong. A result flagged
// isError throws an Error carrying the server's text, so a script can
// catch it and try something else.
//
// Every call is checked against the sandbox policy as
// "mcp.<server>.<tool>" with the tool's original name, not the camelCased
// one. A read-only tool is allowed by DefaultPolicy; any other is denied
// unless granted, and a denial becomes a question for the user when an
// Approver is set — the shape WorkspacePack uses for writes. The answer
// is remembered for the rest of this pack's life.
//
// serverName becomes part of the require() path and of the policy name,
// so it is limited to letters, digits, '-' and '_': a '.' would make
// "mcp.<server>.*" ambiguous.
func MCPPack(ctx context.Context, serverName string, sess MCPSession, cfg MCPConfig) (sandbox.Pack, error) {
	if !mcpServerRe.MatchString(serverName) {
		return sandbox.Pack{}, fmt.Errorf("ext: MCPPack: server name %q must be non-empty and use only letters, digits, '-' and '_'", serverName)
	}
	if sess == nil {
		return sandbox.Pack{}, fmt.Errorf("ext: MCPPack: nil session for server %q", serverName)
	}
	listed, err := sess.ListTools(ctx)
	if err != nil {
		return sandbox.Pack{}, fmt.Errorf("ext: MCPPack: listing tools of %q: %w", serverName, err)
	}
	if len(listed) == 0 {
		return sandbox.Pack{}, fmt.Errorf("ext: MCPPack: server %q offers no tools", serverName)
	}

	used := map[string]bool{}
	tools := make([]mcpTool, 0, len(listed))
	for _, t := range listed {
		if cfg.IgnoreReadOnlyHint {
			t.ReadOnly = false
		}
		tools = append(tools, mcpTool{MCPTool: t, fn: dedupeName(mcpFuncName(t.Name), used)})
	}

	module := "mcp/" + serverName
	description := cfg.Description
	if description == "" {
		description = fmt.Sprintf("Tools of the %s MCP server (%d)", serverName, len(tools))
	}
	decls := renderMCPModule(module, tools)

	m := &mcpServer{ctx: ctx, name: serverName, sess: sess, approver: cfg.Approver, tools: map[string]mcpTool{}, allowed: map[string]bool{}}
	for _, t := range tools {
		m.tools[t.Name] = t
	}

	return sandbox.Pack{
		Name:        module,
		Description: description,
		Prompt:      mcpPrompt(module, description, tools, decls, cfg.inlineTools()),
		HelpEntries: map[string]string{module: decls},
		Register: func(rt *goja.Runtime, sb *sandbox.Sandbox) {
			m.rt, m.sb = rt, sb
			// One global per server rather than a shared one, so two MCP
			// packs in a sandbox cannot overwrite each other's bridge.
			bridge := "_mcp_" + strings.NewReplacer("-", "_").Replace(serverName)
			_ = rt.Set(bridge, m.call)
			sb.AddSkillCode(module, renderMCPModuleJS(bridge, tools))
		},
	}, nil
}

type mcpTool struct {
	MCPTool
	fn string // the camelCased, deduplicated JS name
}

type mcpServer struct {
	ctx      context.Context
	name     string
	sess     MCPSession
	approver sandbox.InputRequester
	tools    map[string]mcpTool // by the server's own name
	rt       *goja.Runtime
	sb       *sandbox.Sandbox

	mu      sync.Mutex
	allowed map[string]bool // tools the user approved this session
}

// call is the one Go function behind every generated tool function. It
// returns {ok, v} rather than throwing for isError, so the JS wrapper
// can throw a real Error — a Go error would reach the script as an
// opaque GoError.
func (m *mcpServer) call(fc goja.FunctionCall) goja.Value {
	tool := fc.Argument(0).String()
	t, ok := m.tools[tool]
	if !ok {
		m.throw("unknown tool %q", tool)
	}
	args := map[string]any{}
	if v := fc.Argument(1); !goja.IsUndefined(v) && !goja.IsNull(v) {
		exported, isMap := v.Export().(map[string]any)
		if !isMap {
			m.throw("%s: the argument must be one object, e.g. %s({ ... })", t.fn, t.fn)
		}
		args = exported
	}

	policyName := "mcp." + m.name + "." + t.Name
	m.authorize(policyName, t, args)

	res, err := m.sess.CallTool(m.ctx, t.Name, args)
	if err != nil {
		m.throw("%s: %v", policyName, err)
	}
	if res.IsError {
		msg := strings.TrimSpace(res.Text)
		if msg == "" {
			msg = "the tool reported an error and gave no message"
		}
		return m.rt.ToValue(map[string]any{"ok": false, "v": msg})
	}
	return m.rt.ToValue(map[string]any{"ok": true, "v": mcpValue(res)})
}

// mcpValue picks the value a script sees. See MCPPack for why only
// objects and arrays are parsed out of text.
func mcpValue(res MCPResult) any {
	if res.Structured != nil {
		return res.Structured
	}
	trimmed := strings.TrimSpace(res.Text)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		var v any
		if json.Unmarshal([]byte(trimmed), &v) == nil {
			return v
		}
	}
	return res.Text
}

func (m *mcpServer) throw(format string, args ...any) {
	panic(m.rt.NewGoError(fmt.Errorf(format, args...)))
}

// authorize gates a call: policy first, so a deployment that grants the
// server never prompts, then the user when the policy said no.
//
// The question names the tool, not the arguments: the prompter recalls an
// answer by its message, so a message carrying arguments would ask again
// for every distinct call and teach people to approve without reading.
func (m *mcpServer) authorize(policyName string, t mcpTool, args map[string]any) {
	m.mu.Lock()
	approved := m.allowed[t.Name]
	m.mu.Unlock()
	if approved {
		return
	}

	res, err := m.sb.CheckPolicy(m.ctx, policyName, map[string]any{
		"server":    m.name,
		"tool":      t.Name,
		"readOnly":  t.ReadOnly,
		"arguments": args,
	})
	if err != nil {
		m.throw("%s: policy check failed: %v", policyName, err)
	}
	if res.Allowed {
		return
	}

	reason := res.Reason
	if reason == "" {
		reason = "denied by policy"
	}
	if m.approver == nil {
		m.sb.Emit(sandbox.Event{Kind: sandbox.EventError, Summary: policyName + " blocked by policy", Detail: reason})
		m.throw("%s: %s", policyName, reason)
	}

	question := fmt.Sprintf("Allow the agent to call %s on the %q MCP server?", t.Name, m.name)
	if first := firstLine(t.Description); first != "" {
		question += " (" + first + ")"
	}
	m.sb.Emit(sandbox.Event{Kind: sandbox.EventAsk, Summary: "requesting permission to call " + policyName})
	value, err := m.approver.Request(sandbox.InputRequestData{InputType: "confirm", Message: question})
	if err != nil {
		m.sb.Emit(sandbox.Event{Kind: sandbox.EventError, Summary: "permission request failed", Detail: err.Error()})
		m.throw("%s: %s (could not ask: %v)", policyName, reason, err)
	}
	if yes, ok := value.(bool); !ok || !yes {
		m.sb.Emit(sandbox.Event{Kind: sandbox.EventAskResult, Summary: "denied: " + policyName})
		m.throw("%s: the user declined to allow this call", policyName)
	}
	m.sb.Emit(sandbox.Event{Kind: sandbox.EventAskResult, Summary: "approved: " + policyName})
	m.mu.Lock()
	m.allowed[t.Name] = true
	m.mu.Unlock()
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
