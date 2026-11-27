package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// serve_config.go is the file `agentloop serve` is configured by.
//
// It is JSON because the standard library reads it: a service that exists so
// non-Go backends need not write Go should not make its operator learn a
// config dependency first. Unknown fields are an error, because the usual
// failure of a config file is a typo that silently turns a restriction off
// ("urlAllowPrefix" for "urlAllowPrefixes" would leave fetch wide open).
//
// Secrets never appear here. Every credential is named by the environment
// variable that holds it, so the file can be committed and shown in a bug
// report without a rotation afterwards.

// serveConfig is the whole file.
type serveConfig struct {
	// Listen is host:port. Loopback by default: a service that can run
	// scripts should not be reachable from the network by accident.
	Listen string `json:"listen"`

	// Store is the SQLite file sessions and steps live in.
	Store string `json:"store"`

	// TokenEnv names the environment variable holding the bearer token.
	TokenEnv string `json:"tokenEnv"`

	// MaxIterations caps LLM round-trips per run; zero is the engine default.
	MaxIterations int `json:"maxIterations"`

	Policy serveConfigPolicy `json:"policy"`

	// OpenAPI turns each entry into a require()-able skill.
	OpenAPI []serveConfigOpenAPI `json:"openapi"`

	// Workspace gives the agent a directory to read and edit; nil means no
	// filesystem at all.
	Workspace *serveConfigWorkspace `json:"workspace"`

	// Capabilities selects among the built-ins (markdown, http, fetch, ai,
	// ask, exec). Nil means the defaults; an empty list means none beyond
	// the always-on core. The workspace and openapi sections install what
	// they describe and are not listed here.
	Capabilities []string `json:"capabilities"`

	// MCP lists MCP servers whose tools become require('mcp/<name>').
	// Granting their side-effecting tools is policy.allowTools, with
	// "mcp.<name>.*" or a single "mcp.<name>.<tool>".
	MCP []serveConfigMCP `json:"mcp"`
}

// serveConfigMCP is one server: Command (stdio) or URL (streamable HTTP).
// Secrets follow the file's rule: Env lists variable NAMES the child may
// inherit, HeadersEnv maps a header to the variable holding its value.
type serveConfigMCP struct {
	Name       string            `json:"name"`
	Command    []string          `json:"command"`
	Env        []string          `json:"env"`
	URL        string            `json:"url"`
	HeadersEnv map[string]string `json:"headersEnv"`
	// IgnoreReadOnlyHint treats every tool as a side effect, for a server
	// that is not trusted to label its own tools.
	IgnoreReadOnlyHint bool `json:"ignoreReadOnlyHint"`
}

// serveConfigPolicy mirrors sandbox.DefaultPolicy field for field, so the
// library's documentation of each setting applies unchanged.
type serveConfigPolicy struct {
	AllowTools           []string `json:"allowTools"`
	URLAllowPrefixes     []string `json:"urlAllowPrefixes"`
	AllowCommands        []string `json:"allowCommands"`
	AllowPrivateNetworks bool     `json:"allowPrivateNetworks"`
}

type serveConfigOpenAPI struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Spec is a path to an OpenAPI 3 document (YAML or JSON).
	Spec    string `json:"spec"`
	BaseURL string `json:"baseURL"`
	// HeadersEnv maps a request header to the env var holding its value.
	// ext.OpenAPIConfig.Headers takes literal values; this is the by-name
	// form, resolved when the server starts.
	HeadersEnv map[string]string `json:"headersEnv"`
	Methods    []string          `json:"methods"`
}

type serveConfigWorkspace struct {
	Root string `json:"root"`
}

const (
	defaultServeListen   = "127.0.0.1:8080"
	defaultServeStore    = "thinking-core.db"
	defaultServeTokenEnv = "THINKING_CORE_TOKEN"
)

// defaultServeCapabilities is what a config that does not say gets: the
// default bundle, with ask() because a run can always be answered over the
// API. exec is never a default — it needs a workspace and a conscious choice.
var defaultServeCapabilities = []string{"markdown", "http", "fetch", "ai", "ask"}

var knownServeCapabilities = append(slices.Clone(defaultServeCapabilities), "exec")

// parseServeConfig reads and validates a config. Relative paths inside it
// resolve against baseDir — the config's own directory, not the process's
// working directory — so the same file works under systemd, in a container
// and from a shell.
func parseServeConfig(r io.Reader, baseDir string) (*serveConfig, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var cfg serveConfig
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	// A second value after the object is almost always a paste accident.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("config: unexpected data after the JSON object")
	}
	if err := cfg.resolve(baseDir); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func loadServeConfig(path string) (*serveConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return parseServeConfig(f, filepath.Dir(abs))
}

func (c *serveConfig) resolve(baseDir string) error {
	if c.Listen == "" {
		c.Listen = defaultServeListen
	}
	if c.Store == "" {
		c.Store = defaultServeStore
	}
	if c.TokenEnv == "" {
		c.TokenEnv = defaultServeTokenEnv
	}
	c.Store = relTo(baseDir, c.Store)
	if c.MaxIterations < 0 {
		return errors.New("config: maxIterations cannot be negative")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return fmt.Errorf("config: listen %q: %w", c.Listen, err)
	}

	if c.Capabilities == nil {
		c.Capabilities = slices.Clone(defaultServeCapabilities)
	}
	for _, name := range c.Capabilities {
		if !slices.Contains(knownServeCapabilities, name) {
			return fmt.Errorf("config: unknown capability %q (known: %s)", name, strings.Join(knownServeCapabilities, ", "))
		}
	}
	if c.Workspace != nil {
		if c.Workspace.Root == "" {
			return errors.New("config: workspace needs a root directory (or null for no workspace)")
		}
		c.Workspace.Root = relTo(baseDir, c.Workspace.Root)
	}
	if slices.Contains(c.Capabilities, "exec") && c.Workspace == nil {
		return errors.New(`config: capability "exec" runs in the workspace, so a workspace section is required`)
	}

	mcpSeen := map[string]bool{}
	for i, m := range c.MCP {
		switch {
		case !mcpNameRe.MatchString(m.Name):
			return fmt.Errorf("config: mcp[%d] name %q may use only letters, digits, '-' and '_'", i, m.Name)
		case mcpSeen[m.Name]:
			return fmt.Errorf("config: mcp name %q appears twice", m.Name)
		case (len(m.Command) == 0) == (m.URL == ""):
			return fmt.Errorf("config: mcp %q needs exactly one of command (stdio) or url (http)", m.Name)
		case m.URL != "" && len(m.Env) > 0:
			return fmt.Errorf("config: mcp %q: env applies to a command, not a url", m.Name)
		case m.URL == "" && len(m.HeadersEnv) > 0:
			return fmt.Errorf("config: mcp %q: headersEnv applies to a url, not a command", m.Name)
		}
		mcpSeen[m.Name] = true
	}

	seen := map[string]bool{}
	for i := range c.OpenAPI {
		o := &c.OpenAPI[i]
		if o.Spec == "" {
			return fmt.Errorf("config: openapi[%d] needs a spec file", i)
		}
		o.Spec = relTo(baseDir, o.Spec)
		// An empty name is allowed (the pack slugs the spec's title), but two
		// of the same name would shadow each other in require().
		if o.Name != "" {
			if seen[o.Name] {
				return fmt.Errorf("config: openapi name %q appears twice", o.Name)
			}
			seen[o.Name] = true
		}
	}
	return nil
}

func relTo(base, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// isLoopbackListen reports whether addr binds only the local machine. An
// empty host (":8080") binds every interface and is therefore not loopback.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// serveToken resolves the bearer token from the environment. No token is
// acceptable only on loopback, where the caller is another process of the
// same machine; anywhere else it is a refusal to start rather than a warning
// nobody reads, because an unauthenticated script-running endpoint on a
// network is not a configuration, it is an incident.
func serveToken(cfg *serveConfig, getenv func(string) string) (token string, warn string, err error) {
	token = getenv(cfg.TokenEnv)
	if token != "" {
		return token, "", nil
	}
	if !isLoopbackListen(cfg.Listen) {
		return "", "", fmt.Errorf("$%s is not set and %s is not a loopback address — set the token, or listen on 127.0.0.1", cfg.TokenEnv, cfg.Listen)
	}
	return "", fmt.Sprintf("$%s is not set: the API is unauthenticated, reachable only from this machine", cfg.TokenEnv), nil
}
