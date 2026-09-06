package sandbox

import (
	"fmt"

	"github.com/dop251/goja"
)

// InputRequester is the sandbox-side seam for human-in-the-loop. The
// ask() primitive and the fetch pack's allowUrls auto-prompt both call
// Request — implementations close over the run's HITL transport (a
// terminal, an SSE stream with a reply endpoint, a websocket) and block
// until the user replies.
//
// A nil InputRequester is the documented "no HITL for this run" case:
// FetchPack still requires the type to exist, but an unapproved-domain
// fetch simply fails with a policy-denial error instead of prompting,
// and InputPack documents ask() but errors when it is called.
type InputRequester interface {
	// Request dispatches the prompt and blocks until the user replies
	// (or the underlying transport errors out). The returned value's
	// concrete type depends on InputType:
	//   - "confirm" → bool
	//   - "choice"  → string (the selected option)
	//   - "text"    → string
	Request(req InputRequestData) (any, error)
}

// InputRequestData is the payload InputRequester.Request receives.
type InputRequestData struct {
	// RequestID is a caller-allocated identifier. Optional.
	RequestID string
	// InputType is one of: "confirm", "choice", "text".
	InputType string
	// Message is the question shown to the user.
	Message string
	// Options is required for InputType == "choice".
	Options []string
	// Placeholder is hint text for InputType == "text" (optional).
	Placeholder string
}

// inputTypes is the vocabulary ask() accepts. It is checked before the
// requester is called: a human-loop round trip is the most expensive
// thing a script can do, and a typo in the type is not worth one.
var inputTypes = map[string]bool{"confirm": true, "choice": true, "text": true}

// InputPack exposes ask() — the synchronous human-in-the-loop primitive.
// The script blocks until the user replies; the value it gets back
// depends on the type it asked for: a bool for confirm, a string for
// choice and text.
//
// It is not in DefaultCapabilities, because nothing generic can supply
// the requester: whether there is a human, and how to reach them, is the
// host's knowledge. A host wraps InputPack in a Capability that closes
// over its own transport, the same way the CLI closes over its terminal.
//
// A nil requester registers the prompt only. ask() is still documented
// to the model, so a script written for an interactive host parses and
// runs, and the call errors — which the model sees and can plan around.
// The alternative, leaving ask() undefined, would turn every such script
// into a ReferenceError with nothing to say about why.
func InputPack(requester InputRequester) Pack {
	return Pack{
		Name:        "input",
		Description: "Human-in-the-loop user interaction",
		Prompt: `// --- Interaction ---

interface AskConfirm { type: "confirm"; message: string }
interface AskChoice { type: "choice"; message: string; options: string[] }
interface AskText { type: "text"; message: string; placeholder?: string }
/** Ask the user to confirm — blocks until they reply; returns their yes/no. */
declare function ask(opts: AskConfirm): boolean;
/** Ask the user to pick one of options — blocks; returns the selected option string. */
declare function ask(opts: AskChoice): string;
/** Ask the user for free text — blocks; returns what they typed. */
declare function ask(opts: AskText): string;`,
		HelpEntries: map[string]string{
			"ask": `ask(opts) — Request input from the user. Blocks until response.
  opts.type: "confirm" (yes/no), "choice" (pick from list), "text" (free input)
  opts.message: The question to display
  opts.options: Array of strings (required for choice type)
  opts.placeholder: Hint text (optional, for text type)
  Returns: boolean (confirm), string (choice/text)`,
		},
		Register: func(rt *goja.Runtime, s *Sandbox) {
			if requester == nil {
				_ = rt.Set("ask", func(call goja.FunctionCall) goja.Value {
					panic(rt.NewGoError(fmt.Errorf("ask: no InputRequester configured for this run")))
				})
				return
			}
			_ = rt.Set("ask", func(call goja.FunctionCall) goja.Value {
				req, err := parseAsk(call)
				if err != nil {
					panic(rt.NewGoError(err))
				}

				s.Emit(Event{Kind: EventAsk, Summary: fmt.Sprintf("ask (%s): %s", req.InputType, req.Message), Detail: req.Message})
				value, err := requester.Request(req)
				if err != nil {
					s.Emit(Event{Kind: EventError, Summary: fmt.Sprintf("ask: %s", err.Error())})
					panic(rt.NewGoError(fmt.Errorf("ask: %w", err)))
				}
				s.Emit(Event{Kind: EventAskResult, Summary: fmt.Sprintf("ask result: %v", value), Result: fmt.Sprintf("%v", value)})
				return rt.ToValue(value)
			})
		},
	}
}

// parseAsk validates the options object the script passed. Every error
// here is one the script can fix on its next turn, so each names the
// field and what it expects rather than reporting a type mismatch.
func parseAsk(call goja.FunctionCall) (InputRequestData, error) {
	if len(call.Arguments) == 0 || goja.IsUndefined(call.Argument(0)) {
		return InputRequestData{}, fmt.Errorf("ask: options object required")
	}
	opts, ok := call.Argument(0).Export().(map[string]any)
	if !ok {
		return InputRequestData{}, fmt.Errorf("ask: options must be an object")
	}

	inputType, _ := opts["type"].(string)
	message, _ := opts["message"].(string)
	if inputType == "" {
		return InputRequestData{}, fmt.Errorf("ask: type is required (confirm, choice, text)")
	}
	if message == "" {
		return InputRequestData{}, fmt.Errorf("ask: message is required")
	}
	if !inputTypes[inputType] {
		return InputRequestData{}, fmt.Errorf("ask: type must be confirm, choice, or text")
	}

	var options []string
	if arr, ok := opts["options"].([]any); ok {
		for _, v := range arr {
			if str, ok := v.(string); ok {
				options = append(options, str)
			}
		}
	}
	if inputType == "choice" && len(options) == 0 {
		return InputRequestData{}, fmt.Errorf("ask: choice type requires options array")
	}
	placeholder, _ := opts["placeholder"].(string)

	return InputRequestData{
		InputType:   inputType,
		Message:     message,
		Options:     options,
		Placeholder: placeholder,
	}, nil
}
