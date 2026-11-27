package bench

import (
	"context"
	"fmt"
	"sync"

	"github.com/mind-vm/thinking-core/agentloop"
	"github.com/mind-vm/thinking-core/agentloopmem"
	"github.com/mind-vm/thinking-core/ext"
	"github.com/mind-vm/thinking-core/llm"
	"github.com/mind-vm/thinking-core/sandbox"
)

// CodeAction is the thinking-core side: an agentloop.Loop whose only
// action is a JavaScript script, with the fixture tools reachable as
// require('mcp/data') through ext.MCPPack — the path a library user
// takes to give the loop someone else's tools, so what is measured is
// the shipped mechanism, not a pack written to win the benchmark.
//
// It uses the loop's default prompts and defaults throughout; only the
// turn cap and timeout are set, to the same values the baseline gets.
type CodeAction struct {
	Endpoint Endpoint
}

func (CodeAction) Name() string { return "thinking-core" }

func (a CodeAction) Run(ctx context.Context, model string, d *Data, task Task) Outcome {
	client, err := llm.NewOpenAI(llm.Config{
		APIKey:     a.Endpoint.APIKey,
		BaseURL:    a.Endpoint.BaseURL,
		ChatModel:  model,
		HTTPClient: a.Endpoint.HTTPClient,
	})
	if err != nil {
		return Outcome{Err: err}
	}
	counted := &countingClient{inner: client}
	tools := &mcpFixture{data: d}

	sessions := agentloopmem.NewSessionStore(nil)
	sessions.Put(agentloop.Session{ID: "bench", SystemPrompt: Persona, Model: model})
	loop := agentloop.New(agentloop.Config{
		LLM:      counted,
		Sessions: sessions,
		Steps:    agentloopmem.NewStepStore(),
		SandboxBuilder: &agentloop.DefaultSandboxBuilder{Capabilities: []agentloop.Capability{
			{
				Name:     "require",
				AlwaysOn: true,
				Build: func(bc agentloop.BuildContext) ([]sandbox.Pack, error) {
					return []sandbox.Pack{sandbox.RequirePack(bc.Ctx)}, nil
				},
			},
			{
				Name: "data",
				Build: func(bc agentloop.BuildContext) ([]sandbox.Pack, error) {
					p, err := ext.MCPPack(bc.Ctx, "data", tools, ext.MCPConfig{Description: "The company's CRM, billing and product-usage data"})
					if err != nil {
						return nil, err
					}
					return []sandbox.Pack{p}, nil
				},
			},
		}},
		// Stated rather than defaulted: it is what allows the read-only
		// fixture tools, and the default logs a notice on every run.
		Policy:        sandbox.DefaultPolicy{},
		Model:         model,
		MaxIterations: a.Endpoint.maxTurns(),
		RunTimeout:    a.Endpoint.runTimeout(),
	})

	res, err := loop.Run(ctx, agentloop.RunRequest{SessionID: "bench", Message: task.Prompt})
	out := counted.outcome()
	out.ToolCalls = tools.calls()
	out.Final = res.FinalText
	switch {
	case err != nil:
		out.Err = err
	case res.Status != "completed":
		out.Err = fmt.Errorf("run ended with status %q", res.Status)
	}
	return out
}

// countingClient counts round-trips and usage at the client, not from
// RunResult, so both agents are metered at the same place: what crossed
// the wire.
type countingClient struct {
	inner llm.Client

	mu               sync.Mutex
	turns, in, compl int
}

func (c *countingClient) Name() string { return c.inner.Name() }

func (c *countingClient) Complete(ctx context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	r, err := c.inner.Complete(ctx, req)
	c.add(r)
	return r, err
}

func (c *countingClient) Stream(ctx context.Context, req llm.CompletionRequest, onChunk func(string)) (llm.CompletionResponse, error) {
	r, err := c.inner.Stream(ctx, req, onChunk)
	c.add(r)
	return r, err
}

func (c *countingClient) add(r llm.CompletionResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.turns++
	c.in += r.InputTokens
	c.compl += r.OutputTokens
}

func (c *countingClient) outcome() Outcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Outcome{Turns: c.turns, PromptTokens: c.in, CompletionTokens: c.compl}
}

// mcpFixture serves the fixture tools as an ext.MCPSession. Every tool is
// read-only, which DefaultPolicy allows without asking.
type mcpFixture struct {
	data *Data

	mu sync.Mutex
	n  int
}

func (m *mcpFixture) ListTools(context.Context) ([]ext.MCPTool, error) {
	out := make([]ext.MCPTool, 0, len(Tools()))
	for _, t := range Tools() {
		out = append(out, ext.MCPTool{Name: t.Name, Description: t.Description, InputSchema: t.Schema, ReadOnly: true})
	}
	return out, nil
}

func (m *mcpFixture) CallTool(_ context.Context, name string, args map[string]any) (ext.MCPResult, error) {
	m.mu.Lock()
	m.n++
	m.mu.Unlock()
	for _, t := range Tools() {
		if t.Name == name {
			v, err := t.Call(m.data, args)
			if err != nil {
				return ext.MCPResult{IsError: true, Text: err.Error()}, nil
			}
			return ext.MCPResult{Structured: v}, nil
		}
	}
	return ext.MCPResult{}, fmt.Errorf("unknown tool %q", name)
}

func (m *mcpFixture) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n
}
