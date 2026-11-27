package cli

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/mind-vm/thinking-core/ext"
)

func stubOpener(context.Context, MCPSpec) (ext.MCPSession, io.Closer, error) {
	return nil, nil, nil
}

func TestParseMCPSpec(t *testing.T) {
	cases := []struct {
		in   string
		want MCPSpec
		bad  string
	}{
		{in: "fs=npx -y server /tmp", want: MCPSpec{Name: "fs", Command: []string{"npx", "-y", "server", "/tmp"}}},
		{in: "gh=https://example.com/mcp", want: MCPSpec{Name: "gh", URL: "https://example.com/mcp"}},
		{in: "local=http://127.0.0.1:8080/mcp", want: MCPSpec{Name: "local", URL: "http://127.0.0.1:8080/mcp"}},
		{in: "nocommand", bad: "want name=command"},
		{in: "x=", bad: "want name=command"},
		{in: "bad.name=cmd", bad: "letters, digits"},
		{in: "u=https://a b", bad: "spaces"},
	}
	for _, c := range cases {
		got, err := parseMCPSpec(c.in)
		if c.bad != "" {
			if err == nil || !strings.Contains(err.Error(), c.bad) {
				t.Errorf("%q: err = %v, want containing %q", c.in, err, c.bad)
			}
			continue
		}
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %+v, %v; want %+v", c.in, got, err, c.want)
		}
	}
}

// The stock build links no MCP SDK, so --mcp must say so at startup
// rather than fail on the first tool call.
func TestResolveMCPWithoutSupport(t *testing.T) {
	saved := hooks
	defer func() { hooks = saved }()
	hooks = Hooks{}
	o := options{mcp: mcpFlag{"fs=server"}}
	err := o.resolveMCP()
	if err == nil || !strings.Contains(err.Error(), "MCP support") {
		t.Errorf("err = %v", err)
	}
}

func TestResolveMCPGrantsAndConflicts(t *testing.T) {
	saved := hooks
	defer func() { hooks = saved }()
	hooks = Hooks{OpenMCP: stubOpener}

	o := options{mcp: mcpFlag{"fs=server", "gh=https://x/mcp"}, allowMCP: "gh"}
	if err := o.resolveMCP(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(o.mcpGrants, []string{"mcp.gh.*"}) {
		t.Errorf("grants = %v", o.mcpGrants)
	}

	for name, o := range map[string]options{
		"duplicate":       {mcp: mcpFlag{"a=x", "a=y"}},
		"unknown grant":   {mcp: mcpFlag{"a=x"}, allowMCP: "b"},
		"http no-network": {mcp: mcpFlag{"a=https://x"}, noNetwork: true},
	} {
		if err := o.resolveMCP(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
