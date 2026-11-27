package bench

import (
	"context"
	"net/http"
	"time"
)

// Persona is the one domain sentence both agents get: thinking-core as
// its session persona, the baseline as its system prompt. Everything
// else each side is told is that side's own idiomatic protocol.
const Persona = "You answer questions about a software company's customers, billing and product usage, using the data tools available to you. " +
	"Compute exact answers from the data; never estimate, sample or guess. Fetch every page you need."

// Outcome is what one agent run produced, before it is checked.
type Outcome struct {
	Final            string
	Turns            int // LLM round-trips
	ToolCalls        int // fixture tool invocations
	PromptTokens     int
	CompletionTokens int
	Err              error
}

// Agent is one side of the comparison.
type Agent interface {
	Name() string
	Run(ctx context.Context, model string, d *Data, task Task) Outcome
}

// Endpoint is the OpenAI-wire host both agents talk to, so the same model
// at the same address answers for both.
type Endpoint struct {
	BaseURL string
	APIKey  string
	// HTTPClient is optional; tests pass the scripted server's client.
	HTTPClient *http.Client
	// MaxTurns caps LLM round-trips per run, identically for both sides.
	MaxTurns int
	// RunTimeout caps one run's wall time.
	RunTimeout time.Duration
}

func (e Endpoint) maxTurns() int {
	if e.MaxTurns <= 0 {
		return 20
	}
	return e.MaxTurns
}

func (e Endpoint) runTimeout() time.Duration {
	if e.RunTimeout <= 0 {
		return 10 * time.Minute
	}
	return e.RunTimeout
}
