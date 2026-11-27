// Command agentloop is the agentloop CLI with MCP support: the same
// command as cmd/agentloop, plus --mcp.
//
// It exists as a separate main in the ext/mcp module because the stock
// binary is built from the root module, which must not depend on the MCP
// SDK (ADR-0018). Everything else is the shared cli package; this file only
// supplies the one thing the root build leaves nil.
//
//	go install github.com/mind-vm/thinking-core/ext/mcp/cmd/agentloop@latest
//	agentloop --mcp 'fs=npx -y @modelcontextprotocol/server-filesystem /tmp' "list /tmp"
package main

import (
	"context"
	"io"
	"os"

	"github.com/mind-vm/thinking-core/cmd/agentloop/cli"
	"github.com/mind-vm/thinking-core/ext"
	"github.com/mind-vm/thinking-core/ext/mcp"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], cli.Hooks{OpenMCP: open}))
}

// open dials one MCP server. A stdio server inherits only the baseline
// environment plus the variables its spec names: the --mcp flag names none,
// which keeps the flag honest, while `serve`'s config file can list them.
func open(ctx context.Context, spec cli.MCPSpec) (ext.MCPSession, io.Closer, error) {
	var (
		s   *mcp.Session
		err error
	)
	if spec.URL != "" {
		s, err = mcp.ConnectHTTP(ctx, mcp.HTTPConfig{URL: spec.URL, Headers: spec.Headers})
	} else {
		s, err = mcp.ConnectStdio(ctx, mcp.StdioConfig{Command: spec.Command[0], Args: spec.Command[1:], Env: spec.Env, Stderr: os.Stderr})
	}
	if err != nil {
		return nil, nil, err
	}
	return s, s, nil
}
