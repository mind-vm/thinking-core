package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/mind-vm/thinking-core/agentloop"
	"github.com/mind-vm/thinking-core/ext"
	"github.com/mind-vm/thinking-core/sandbox"
)

// mcpFlag collects every --mcp occurrence; the flag is repeatable.
type mcpFlag []string

func (f *mcpFlag) String() string     { return strings.Join(*f, "; ") }
func (f *mcpFlag) Set(v string) error { *f = append(*f, v); return nil }

var mcpNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// parseMCPSpec parses `name=command arg...` (stdio) or `name=https://...`
// (streamable HTTP).
//
// The command is split on whitespace, with no quoting: a path with a
// space, or an argument that needs one, is not expressible. That is a
// deliberate limit of a one-line flag — the alternative is to invent a
// shell dialect here — and anything that needs more belongs in the
// library API (or a config file) where arguments are a list already.
func parseMCPSpec(raw string) (MCPSpec, error) {
	name, rest, ok := strings.Cut(raw, "=")
	rest = strings.TrimSpace(rest)
	if !ok || rest == "" {
		return MCPSpec{}, fmt.Errorf("--mcp %q: want name=command [args...] or name=https://url", raw)
	}
	if !mcpNameRe.MatchString(name) {
		return MCPSpec{}, fmt.Errorf("--mcp %q: the server name may use only letters, digits, '-' and '_'", raw)
	}
	if strings.HasPrefix(rest, "http://") || strings.HasPrefix(rest, "https://") {
		if strings.ContainsAny(rest, " \t") {
			return MCPSpec{}, fmt.Errorf("--mcp %q: a URL cannot contain spaces", raw)
		}
		return MCPSpec{Name: name, URL: rest}, nil
	}
	return MCPSpec{Name: name, Command: strings.Fields(rest)}, nil
}

// resolveMCP validates the --mcp and --allow-mcp flags.
func (o *options) resolveMCP() error {
	seen := map[string]bool{}
	for _, raw := range o.mcp {
		spec, err := parseMCPSpec(raw)
		if err != nil {
			return err
		}
		if seen[spec.Name] {
			return fmt.Errorf("--mcp: the server name %q is used twice", spec.Name)
		}
		seen[spec.Name] = true
		// --no-network promises nothing can leave the machine. An HTTP
		// server is a plain breach of that; a stdio server may still reach
		// the network on its own, which a flag here cannot see, so the
		// help text says so rather than this code pretending otherwise.
		if o.noNetwork && spec.URL != "" {
			return fmt.Errorf("--mcp %s: --no-network removes network access, and %s is a network server", spec.Name, spec.URL)
		}
		o.mcpSpecs = append(o.mcpSpecs, spec)
	}
	if len(o.mcpSpecs) > 0 && hooks.OpenMCP == nil {
		return errors.New("--mcp needs a build with MCP support: this agentloop does not link the MCP SDK (ADR-0018). " +
			"Install github.com/mind-vm/thinking-core/ext/mcp/cmd/agentloop instead")
	}
	for _, name := range splitList(o.allowMCP) {
		if !seen[name] {
			return fmt.Errorf("--allow-mcp %s: no --mcp server of that name", name)
		}
		o.mcpGrants = append(o.mcpGrants, "mcp."+name+".*")
	}
	return nil
}

// mcpCapabilities opens every --mcp server and returns one capability
// each, plus the closers to run at exit.
//
// Servers are opened once here rather than in Capability.Build, which runs
// for every sandbox: a stdio server is a child process, and spawning one
// per run would make the model wait on process start each turn and leave
// the state of a stateful server behind.
func mcpCapabilities(ctx context.Context, o *options, requester sandbox.InputRequester) ([]agentloop.Capability, []io.Closer, error) {
	var caps []agentloop.Capability
	var closers []io.Closer
	for _, spec := range o.mcpSpecs {
		sess, closer, err := hooks.OpenMCP(ctx, spec)
		if err != nil {
			for _, c := range closers {
				_ = c.Close()
			}
			return nil, nil, fmt.Errorf("--mcp %s: %w", spec.Name, err)
		}
		closers = append(closers, closer)
		caps = append(caps, agentloop.Capability{
			Name:        "mcp/" + spec.Name,
			Description: "tools of the " + spec.Name + " MCP server",
			Build: func(bc agentloop.BuildContext) ([]sandbox.Pack, error) {
				pack, err := ext.MCPPack(bc.Ctx, spec.Name, sess, ext.MCPConfig{Approver: requester})
				if err != nil {
					return nil, err
				}
				return []sandbox.Pack{pack}, nil
			},
		})
	}
	return caps, closers, nil
}
