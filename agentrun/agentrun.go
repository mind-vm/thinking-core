// Package agentrun is the transport half of an agent run behind an HTTP API:
// the wire shape of the loop's events, the Server-Sent Events writer that
// carries them, and the broker that connects a script's blocking ask() to a
// person's reply on another connection.
//
// # Why it is here
//
// `agentloop` runs the loop and knows nothing about HTTP. An application that
// exposes a run to a browser has to solve the same three problems every time:
// rename the loop's events for a JSON API, write them as an event stream
// without racing the loop's own goroutines, and hold a half-duplex prompt open
// across two requests. Two applications had each written all three, the same
// way, before this package existed; it is their shared copy.
//
// # What is deliberately not here
//
// The session and step stores. Those belong to the application, over its own
// tables and with its own tenancy rules. What every store must agree on is
// behaviour, and that is `agentlooptest`'s conformance kit.
//
// Nor any router or auth. The package takes an http.ResponseWriter and opaque
// ids, and the caller decides who may open a stream or answer a prompt.
package agentrun

import (
	"github.com/mind-vm/thinking-core/agentloop"
)

// Event is one thing that happened during a run, as it reaches a client.
//
// It is agentloop.RunEvent renamed and re-cased for the wire. The loop's own
// shape uses snake_case in a couple of nested places and a JSON API is
// camelCase throughout; translating once here is cheaper than a client that
// has to know which half of a payload came from where.
type Event struct {
	Type    string         `json:"type" doc:"user, execute_js, execute_js_result, sandbox_event, data_update, compacted, budget_trimmed, response, response_chunk, input_request, warning, error, done"`
	Content string         `json:"content,omitempty"`
	Tool    string         `json:"tool,omitempty"`
	Args    map[string]any `json:"args,omitempty"`
	Tokens  *Tokens        `json:"tokens,omitempty"`
	Summary *Summary       `json:"summary,omitempty"`
}

// Tokens is one call's cost.
type Tokens struct {
	Prompt     int32 `json:"prompt"`
	Completion int32 `json:"completion"`
}

// Summary rides the terminal `done` event.
type Summary struct {
	SessionID        string `json:"sessionId"`
	Steps            int    `json:"steps"`
	Tokens           Tokens `json:"tokens"`
	DataBytesCarried int64  `json:"dataBytesCarried" doc:"Bytes of working state threaded server-side rather than sent to the model"`
}

// FromRunEvent translates the loop's event into the wire's.
func FromRunEvent(e agentloop.RunEvent) Event {
	out := Event{Type: e.Type, Content: e.Content, Tool: e.Tool, Args: e.Args}
	if e.Tokens != nil {
		out.Tokens = &Tokens{Prompt: e.Tokens.Prompt, Completion: e.Tokens.Completion}
	}
	if e.Summary != nil {
		out.Summary = &Summary{
			SessionID: e.Summary.SessionID, Steps: e.Summary.Steps,
			Tokens:           Tokens{Prompt: e.Summary.Tokens.Prompt, Completion: e.Summary.Tokens.Completion},
			DataBytesCarried: e.Summary.DataBytesCarried,
		}
	}
	return out
}

// SummaryOf translates a finished run's result into the terminal summary.
func SummaryOf(res agentloop.RunResult) Summary {
	return Summary{
		SessionID: res.RunID, Steps: res.Steps,
		Tokens:           Tokens{Prompt: res.Tokens.Prompt, Completion: res.Tokens.Completion},
		DataBytesCarried: res.DataBytesCarried,
	}
}
