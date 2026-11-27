// Package mcp is the MCP client behind ext.MCPPack: it connects to an MCP
// server over stdio or streamable HTTP with the official Go SDK and
// presents it as an ext.MCPSession.
//
// It is a separate Go module for the reason browser/chrome is: the SDK and
// its dependencies stay off the dependency list of every deployment that
// never talks to an MCP server. ext defines the small interface; this
// module is one implementation of it.
//
//	sess, err := mcp.ConnectStdio(ctx, mcp.StdioConfig{
//	    Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
//	})
//	if err != nil { ... }
//	defer sess.Close()
//
//	pack, err := ext.MCPPack(ctx, "fs", sess, ext.MCPConfig{})
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mind-vm/thinking-core/ext"
)

// Session is a connected MCP client. It implements ext.MCPSession.
type Session struct {
	cs *sdk.ClientSession
}

var _ ext.MCPSession = (*Session)(nil)

// NewSession wraps an already-connected SDK session, for a transport the
// constructors below do not cover (an in-memory pair in a test, say).
func NewSession(cs *sdk.ClientSession) *Session { return &Session{cs: cs} }

func newClient() *sdk.Client {
	return sdk.NewClient(&sdk.Implementation{Name: "agentloop", Version: "v0"}, nil)
}

// Close ends the session. For a stdio server that closes its stdin and, if
// it does not exit, terminates it — so callers must Close, or leave the
// child process behind.
func (s *Session) Close() error { return s.cs.Close() }

// ListTools returns every tool the server offers, following pagination.
func (s *Session) ListTools(ctx context.Context) ([]ext.MCPTool, error) {
	var out []ext.MCPTool
	for t, err := range s.cs.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		out = append(out, ext.MCPTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schemaMap(t.InputSchema),
			ReadOnly:    t.Annotations != nil && t.Annotations.ReadOnlyHint,
		})
	}
	return out, nil
}

// schemaMap normalises the schema the SDK hands back, which is a
// map[string]any from a client but typed `any`.
func schemaMap(v any) map[string]any {
	switch s := v.(type) {
	case nil:
		return nil
	case map[string]any:
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	return m
}

// CallTool calls one tool. A tool that ran and failed comes back as an
// ext.MCPResult with IsError set; only a protocol or transport failure is
// an error here.
func (s *Session) CallTool(ctx context.Context, name string, args map[string]any) (ext.MCPResult, error) {
	res, err := s.cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return ext.MCPResult{}, err
	}
	return ext.MCPResult{
		Text:       flatten(res.Content),
		Structured: res.StructuredContent,
		IsError:    res.IsError,
	}, nil
}

// flatten joins a result's content blocks into text. Blocks a script
// cannot use — image and audio bytes — become a one-line placeholder
// rather than vanishing, so the model can tell a tool returned something
// it was not shown instead of concluding the result was empty.
func flatten(blocks []sdk.Content) string {
	var parts []string
	for _, b := range blocks {
		switch c := b.(type) {
		case *sdk.TextContent:
			parts = append(parts, c.Text)
		case *sdk.ImageContent:
			parts = append(parts, fmt.Sprintf("[image %s, %d bytes, not shown]", c.MIMEType, len(c.Data)))
		case *sdk.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio %s, %d bytes, not shown]", c.MIMEType, len(c.Data)))
		case *sdk.ResourceLink:
			parts = append(parts, fmt.Sprintf("[resource %s]", c.URI))
		case *sdk.EmbeddedResource:
			switch {
			case c.Resource == nil:
			case c.Resource.Text != "":
				parts = append(parts, c.Resource.Text)
			default:
				parts = append(parts, fmt.Sprintf("[resource %s, %d bytes, not shown]", c.Resource.URI, len(c.Resource.Blob)))
			}
		}
	}
	return strings.Join(parts, "\n")
}

// StdioConfig describes a server run as a child process.
type StdioConfig struct {
	Command string
	Args    []string
	Dir     string

	// Env names the environment variables the child may inherit, beyond a
	// small baseline (see baselineEnv). The default is not "everything":
	// the agent's own process holds API keys for its model, and an MCP
	// server is third-party code that has no business reading them. A
	// server that needs a token is given it by name here.
	Env []string

	// ExtraEnv is set on the child verbatim, for a value that should not
	// come from this process's environment at all.
	ExtraEnv map[string]string

	// Stderr receives the server's stderr. Nil discards it. Servers log
	// there, so pass os.Stderr while debugging one that will not start.
	Stderr io.Writer
}

// baselineEnv is what every stdio server inherits: enough for a launcher
// like npx, uvx or a shell script to find itself and a scratch directory,
// and nothing that is a credential.
var baselineEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TMPDIR", "TEMP", "TMP",
	"SYSTEMROOT", "USERPROFILE", "APPDATA", "LOCALAPPDATA",
}

// ConnectStdio starts the server and completes the MCP handshake.
func ConnectStdio(ctx context.Context, cfg StdioConfig) (*Session, error) {
	if cfg.Command == "" {
		return nil, fmt.Errorf("mcp: stdio server has no command")
	}
	// Not CommandContext: the SDK's transport shuts the process down in
	// order (close stdin, wait, then terminate), and a context kill would
	// cut that short — and would tie the server's life to the ctx of the
	// connect call, which callers typically cancel once connected.
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = cfg.Dir
	cmd.Env = childEnv(os.Environ(), cfg.Env, cfg.ExtraEnv)
	cmd.Stderr = cfg.Stderr

	cs, err := newClient().Connect(ctx, &sdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: starting %s: %w", cfg.Command, err)
	}
	return &Session{cs: cs}, nil
}

// childEnv builds the child's environment from the parent's: baseline and
// allowlisted names that are set, then extra.
func childEnv(parent, allow []string, extra map[string]string) []string {
	want := map[string]bool{}
	for _, n := range baselineEnv {
		want[n] = true
	}
	for _, n := range allow {
		want[n] = true
	}
	var env []string
	for _, kv := range parent {
		if name, _, ok := strings.Cut(kv, "="); ok && want[name] {
			env = append(env, kv)
		}
	}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// HTTPConfig describes a server reached over streamable HTTP.
type HTTPConfig struct {
	URL string

	// Headers are sent on every request — the place for a bearer token.
	Headers map[string]string

	// HTTPClient is the client to use; nil uses a copy of
	// http.DefaultClient. Headers are applied to a wrapper around its
	// Transport, never by mutating the client passed in.
	HTTPClient *http.Client
}

// ConnectHTTP connects to a streamable-HTTP server and completes the MCP
// handshake.
func ConnectHTTP(ctx context.Context, cfg HTTPConfig) (*Session, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("mcp: HTTP server has no URL")
	}
	hc := &http.Client{}
	if cfg.HTTPClient != nil {
		c := *cfg.HTTPClient
		hc = &c
	}
	if len(cfg.Headers) > 0 {
		base := hc.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		hc.Transport = headerTransport{base: base, headers: cfg.Headers}
	}
	cs, err := newClient().Connect(ctx, &sdk.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: hc}, nil)
	if err != nil {
		return nil, fmt.Errorf("mcp: connecting to %s: %w", cfg.URL, err)
	}
	return &Session{cs: cs}, nil
}

// headerTransport adds fixed headers to a clone of each request, since a
// RoundTripper must not modify the request it is given.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	return t.base.RoundTrip(r)
}
