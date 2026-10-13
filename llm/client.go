// Package llm is the LLM client interface agentloop's ai()/aiJSON()
// sandbox capability calls against, and what drives the agent loop's
// own turn-taking.
package llm

import "context"

// Message is one turn of a chat completion request.
type Message struct {
	Role    string // "system", "user", or "assistant"
	Content string

	// ThoughtSignatures are opaque tokens a reasoning model attached to
	// an assistant reply (Gemini 3's thought signatures, carried by
	// OpenAI-compatible proxies in provider_specific_fields). They are
	// sent back with the message unchanged. Gemini 3 needs them across
	// turns: without them it loses its reasoning thread, and after a few
	// turns starts replying with nothing, or with a call to a function
	// it was never offered.
	ThoughtSignatures []string
}

// CompletionRequest is one call to Client.Complete or Client.Stream.
type CompletionRequest struct {
	Messages    []Message
	Model       string // empty uses the client's configured default
	Temperature *float64
	MaxTokens   *int
}

// CompletionResponse is what a completion call returns.
type CompletionResponse struct {
	Content      string
	Model        string
	InputTokens  int
	OutputTokens int

	// Reasoning is the model's thinking, when the upstream returns it
	// (reasoning_content or reasoning). Empty for models that hide it.
	Reasoning string
	// FinishReason is why generation stopped, verbatim from the
	// upstream ("stop", "length", "content_filter", …). "length" on an
	// empty Content means thinking spent the output budget.
	FinishReason string
	// ReasoningTokens is the part of OutputTokens spent thinking, when
	// the upstream reports it.
	ReasoningTokens int
	// ThoughtSignatures are the reply's opaque reasoning tokens, to be
	// stored on the assistant Message and sent back; see
	// Message.ThoughtSignatures.
	ThoughtSignatures []string
}

// Client is what the ai()/aiJSON() sandbox capability, and the agent
// loop's own turn-taking, call against.
type Client interface {
	Name() string
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
	// Stream delivers content deltas through onChunk as they arrive and
	// still returns the full accumulated response — "best-effort
	// incremental, full response always", so a caller that doesn't care
	// about incremental output can pass a nil onChunk and just use the
	// return value.
	Stream(ctx context.Context, req CompletionRequest, onChunk func(chunk string)) (CompletionResponse, error)
}
