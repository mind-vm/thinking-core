package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/google/uuid"

	"github.com/mind-vm/thinking-core/agentloop"
	"github.com/mind-vm/thinking-core/agentrun"
	"github.com/mind-vm/thinking-core/ext"
	"github.com/mind-vm/thinking-core/llm"
	"github.com/mind-vm/thinking-core/sandbox"
)

// cmdServe runs the engine as an HTTP service, so a backend in any language
// can start a run with a POST and read its events as Server-Sent Events.
//
// It is a subcommand rather than a second binary: it shares the LLM
// configuration, the policy and the capability bundle with the CLI, and a
// second binary would be a second thing to release and keep in step.
func cmdServe(argv []string) int {
	var o options
	fs := flag.NewFlagSet("agentloop serve", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to the JSON config file (required)")
	fs.StringVar(&o.model, "model", "", "chat model (default: the client's own, from $OPENAI_CHAT_MODEL)")
	fs.DurationVar(&o.timeout, "timeout", 0, "wall-clock cap per run (0 = engine default)")
	fs.IntVar(&o.historyWindow, "history-window", 0, "prior steps rehydrated into context (0 = engine default)")
	fs.IntVar(&o.contextWindow, "context-window", 0, "the model's SERVED context size in tokens (0 = assume a large window)")
	fs.BoolVar(&o.quiet, "quiet", false, "log errors only")
	if err := fs.Parse(argv); err != nil {
		return exitUsage
	}
	if *configPath == "" || fs.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "agentloop serve: want --config <file>")
		return exitUsage
	}
	if o.timeout < 0 || o.historyWindow < 0 || o.contextWindow < 0 {
		fmt.Fprintln(os.Stderr, "agentloop serve: --timeout, --history-window and --context-window cannot be negative")
		return exitUsage
	}
	configureLogging(&o)

	cfg, err := loadServeConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentloop serve: %s\n", err)
		return exitUsage
	}
	token, warn, err := serveToken(cfg, os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentloop serve: %s\n", err)
		return exitUsage
	}
	if warn != "" {
		slog.Warn(warn)
	}
	o.maxSteps = cfg.MaxIterations

	client, err := llm.NewOpenAI(llm.ConfigFromEnv())
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentloop serve: %s\n", err)
		return exitUsage
	}
	store, err := openStore(&options{db: cfg.Store})
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentloop serve: %s\n", err)
		return exitUsage
	}
	defer store.Close()

	broker := agentrun.NewBroker()
	caps, closeCaps, err := serveCapabilities(cfg, client, o.model, broker, os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentloop serve: %s\n", err)
		return exitUsage
	}
	defer closeCaps()

	loopCfg := o.loopConfig(client, caps, store, store)
	// loopConfig's policy only knows the CLI's --allow-exec; the file carries
	// the whole of DefaultPolicy.
	loopCfg.Policy = cfg.Policy.policy()

	ctx, stop := signalContext()
	defer stop()
	srv := newServeServer(serveDeps{loop: agentloop.New(loopCfg), store: store, broker: broker, token: token, ctx: ctx})

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agentloop serve: %s\n", err)
		return exitUsage
	}
	httpSrv := &http.Server{
		Handler: srv.handler(),
		// Header timeout only: a WriteTimeout would cut every event stream
		// that outlives it, and a ReadTimeout every slow upload of context.
		ReadHeaderTimeout: 10 * time.Second,
	}
	// A plain line rather than slog: logging is at Warn for the CLI's sake,
	// and this is the one thing an operator needs to see at startup.
	fmt.Fprintf(os.Stderr, "agentloop serve: http://%s (store %s, auth %t)\n", ln.Addr(), cfg.Store, token != "")

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()

	select {
	case err := <-errc:
		fmt.Fprintf(os.Stderr, "agentloop serve: %s\n", err)
		return exitError
	case <-ctx.Done():
	}

	// Order matters. Runs are cancelled first (ctx is their parent, already
	// done), because an open SSE stream keeps Shutdown waiting for as long as
	// its run lives; once the runs have written their terminal event the
	// streams drain on their own. The store closes last, deferred, after
	// nothing can write to it.
	srv.wait()
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		_ = httpSrv.Close()
	}
	return exitOK
}

// policy builds the PolicyChecker the file describes.
func (p serveConfigPolicy) policy() sandbox.PolicyChecker {
	return sandbox.DefaultPolicy{
		AllowTools:           p.AllowTools,
		URLAllowPrefixes:     p.URLAllowPrefixes,
		AllowCommands:        p.AllowCommands,
		AllowPrivateNetworks: p.AllowPrivateNetworks,
	}
}

// serveCapabilities assembles what runs may do, from the file.
//
// It starts from agentloop.DefaultCapabilities and filters, as buildCapabilities
// does, so a capability added to the library still reaches the service. The
// difference from the CLI is who answers a permission question: there is no
// terminal, so fetch's domain approval, the workspace's and exec's go through
// the broker like ask() does — as an `input_request` event the caller answers
// on POST /v1/runs/{id}/answer. A backend that does not want to be asked
// narrows the policy instead.
func serveCapabilities(cfg *serveConfig, client llm.Client, model string, broker *agentrun.Broker, getenv func(string) string) (caps []agentloop.Capability, closeFn func(), err error) {
	closeFn = func() {}
	// Packs for one run ask on that run's session; the requester is built from
	// the BuildContext, so each capability closes over the broker, not a session.
	requester := func(bc agentloop.BuildContext) (sandbox.InputRequester, error) {
		sid, err := uuid.Parse(bc.SessionID)
		if err != nil {
			return nil, fmt.Errorf("session id %q is not a uuid", bc.SessionID)
		}
		return broker.Requester(sid), nil
	}

	for _, c := range agentloop.DefaultCapabilities(client, model) {
		// AlwaysOn is the runtime itself (require); everything else is opt-in
		// by name so the file is the whole truth about what a run can do.
		if !c.AlwaysOn && !slices.Contains(cfg.Capabilities, c.Name) {
			continue
		}
		if c.Name == "fetch" {
			c.Build = func(bc agentloop.BuildContext) ([]sandbox.Pack, error) {
				req, err := requester(bc)
				if err != nil {
					return nil, err
				}
				return []sandbox.Pack{sandbox.FetchPack(bc.Ctx, req)}, nil
			}
		}
		caps = append(caps, c)
	}
	if slices.Contains(cfg.Capabilities, "ask") {
		caps = append(caps, agentloop.Capability{
			Name:        "ask",
			Description: "ask the caller a question and wait for the answer",
			Build: func(bc agentloop.BuildContext) ([]sandbox.Pack, error) {
				req, err := requester(bc)
				if err != nil {
					return nil, err
				}
				return []sandbox.Pack{sandbox.InputPack(req)}, nil
			},
		})
	}

	if cfg.Workspace != nil {
		root, err := os.OpenRoot(cfg.Workspace.Root)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot open the workspace %s: %w", cfg.Workspace.Root, err)
		}
		closeFn = func() { root.Close() }
		caps = append(caps, agentloop.Capability{
			Name:        "workspace",
			Description: "read and change files in the workspace directory",
			Build: func(bc agentloop.BuildContext) ([]sandbox.Pack, error) {
				req, err := requester(bc)
				if err != nil {
					return nil, err
				}
				return []sandbox.Pack{ext.WorkspacePack(bc.Ctx, root, ext.WorkspaceOptions{Approver: req})}, nil
			},
		})
		if slices.Contains(cfg.Capabilities, "exec") {
			caps = append(caps, agentloop.Capability{
				Name:        "exec",
				Description: "run commands and read their output",
				Build: func(bc agentloop.BuildContext) ([]sandbox.Pack, error) {
					req, err := requester(bc)
					if err != nil {
						return nil, err
					}
					return []sandbox.Pack{ext.ExecPack(bc.Ctx, root, ext.ExecOptions{Approver: req})}, nil
				},
			})
		}
	}

	for _, oa := range cfg.OpenAPI {
		pack, err := openAPIPack(oa, getenv)
		if err != nil {
			closeFn()
			return nil, nil, err
		}
		caps = append(caps, agentloop.Capability{
			Name:        "openapi:" + pack.Name,
			Description: pack.Description,
			Build:       func(agentloop.BuildContext) ([]sandbox.Pack, error) { return []sandbox.Pack{pack}, nil },
		})
	}

	mcpCaps, closeMCP, err := serveMCP(context.Background(), cfg.MCP, requester, getenv)
	if err != nil {
		closeFn()
		return nil, nil, err
	}
	caps = append(caps, mcpCaps...)
	closeWS := closeFn
	return caps, func() { closeMCP(); closeWS() }, nil
}

// serveMCP opens every configured server once, at startup, and returns one
// capability per server. Opened here rather than in Capability.Build for the
// reason mcpCapabilities gives: a stdio server is a child process, and one per
// run would make every run wait on process start. A server that cannot be
// reached stops the service from coming up rather than failing runs later.
func serveMCP(ctx context.Context, specs []serveConfigMCP, requester func(agentloop.BuildContext) (sandbox.InputRequester, error), getenv func(string) string) ([]agentloop.Capability, func(), error) {
	if len(specs) == 0 {
		return nil, func() {}, nil
	}
	if hooks.OpenMCP == nil {
		return nil, nil, errors.New("config has an mcp section, but this agentloop does not link the MCP SDK (ADR-0018); " +
			"install github.com/mind-vm/thinking-core/ext/mcp/cmd/agentloop instead")
	}
	var caps []agentloop.Capability
	var closers []io.Closer
	closeAll := func() {
		for _, c := range closers {
			_ = c.Close()
		}
	}
	for _, m := range specs {
		spec := MCPSpec{Name: m.Name, Command: m.Command, URL: m.URL, Env: m.Env}
		for header, env := range m.HeadersEnv {
			v := getenv(env)
			if v == "" {
				closeAll()
				return nil, nil, fmt.Errorf("mcp %s: header %s comes from $%s, which is not set", m.Name, header, env)
			}
			if spec.Headers == nil {
				spec.Headers = map[string]string{}
			}
			spec.Headers[header] = v
		}
		sess, closer, err := hooks.OpenMCP(ctx, spec)
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("mcp %s: %w", m.Name, err)
		}
		closers = append(closers, closer)
		ignore := m.IgnoreReadOnlyHint
		caps = append(caps, agentloop.Capability{
			Name:        "mcp/" + m.Name,
			Description: "tools of the " + m.Name + " MCP server",
			Build: func(bc agentloop.BuildContext) ([]sandbox.Pack, error) {
				req, err := requester(bc)
				if err != nil {
					return nil, err
				}
				pack, err := ext.MCPPack(bc.Ctx, spec.Name, sess, ext.MCPConfig{Approver: req, IgnoreReadOnlyHint: ignore})
				if err != nil {
					return nil, err
				}
				return []sandbox.Pack{pack}, nil
			},
		})
	}
	return caps, closeAll, nil
}

// openAPIPack loads one spec and resolves its header secrets. Both happen at
// startup, not per run: a missing env var or an unparseable spec should stop
// the service from coming up, not fail the first run that needs it.
func openAPIPack(oa serveConfigOpenAPI, getenv func(string) string) (sandbox.Pack, error) {
	spec, err := openapi3.NewLoader().LoadFromFile(oa.Spec)
	if err != nil {
		return sandbox.Pack{}, fmt.Errorf("openapi %s: %w", oa.Spec, err)
	}
	var headers map[string]string
	for header, env := range oa.HeadersEnv {
		v := getenv(env)
		if v == "" {
			return sandbox.Pack{}, fmt.Errorf("openapi %s: header %s comes from $%s, which is not set", oa.Spec, header, env)
		}
		if headers == nil {
			headers = map[string]string{}
		}
		headers[header] = v
	}
	pack, err := ext.OpenAPIPack(spec, ext.OpenAPIConfig{
		Name: oa.Name, Description: oa.Description, BaseURL: oa.BaseURL, Headers: headers, Methods: oa.Methods,
	})
	if err != nil {
		return sandbox.Pack{}, fmt.Errorf("openapi %s: %w", oa.Spec, err)
	}
	return pack, nil
}
