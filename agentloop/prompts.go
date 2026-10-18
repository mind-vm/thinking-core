package agentloop

import "strings"

// TextEmissionSystemPrompt is the workflow contract the loop layers on
// top of the sandbox's primitive documentation: each reply is one fenced
// ```javascript block that runs; what the script leaves in `state` is
// threaded into the next turn (full fidelity, server-side — the model
// sees only a shape digest of it plus its own log() output).
//
// It states only what the model cannot know — that a reply runs, what
// persists, what the runtime lacks — and leaves the rest to how it would
// write a script anyway. Every line is a fact about this sandbox, not a
// convention to learn; that is what keeps it to a few lines. Primitive
// listings come from the packs via sandbox.Sandbox.SystemPrompt() and
// are appended under "## Sandbox API".
//
// The runtime line is empirically grounded: goja parses Promise/async
// code and then its continuations silently never run, so the prompt
// rules them out rather than leaving the model to find out.
func TextEmissionSystemPrompt() string {
	return textEmissionPrompt(false)
}

// textEmissionPrompt names the finish the loop wired: print() and exit()
// under Config.UserOutput, answer() otherwise.
func textEmissionPrompt(userOutput bool) string {
	finish := "you call answer(result)"
	if userOutput {
		finish = "you print() the result for the user and call exit()"
	}
	return strings.Replace(textEmissionTemplate, "{{finish}}", finish, 1)
}

const textEmissionTemplate = "You solve tasks by writing JavaScript. Reply with one ```javascript block; it runs in a sandbox, and you see what it logs. Continue with the next block until {{finish}}.\n\n" +
	"- `state` is an object that persists between your scripts. Keep data there; you are shown its shape, not its values.\n" +
	"- log() what you need to read yourself: counts, names, small samples, not whole datasets.\n" +
	"- The runtime is synchronous: no await or Promises, no Node or browser APIs. The functions declared below are all there is."

// ComposeSystemPrompt builds the full system prompt the loop sends to
// the LLM for a session: the text-emission contract, then the optional
// persona, then the sandbox's primitive documentation. Persona AFTER
// protocol, sandbox API LAST so declarations sit close to the user
// message.
func ComposeSystemPrompt(persona, sandboxAPI string) string {
	return composeSystemPrompt("", false, persona, sandboxAPI)
}

// composeSystemPrompt is ComposeSystemPrompt with Config.Protocol in place
// of the default contract when one is set, and the default contract's
// finish matching Config.UserOutput when not.
func composeSystemPrompt(protocol string, userOutput bool, persona, sandboxAPI string) string {
	out := protocol
	if out == "" {
		out = textEmissionPrompt(userOutput)
	}
	if persona != "" {
		out += "\n\n## Persona\n\n" + persona
	}
	if sandboxAPI != "" {
		out += "\n\n## Sandbox API\n\n" + sandboxAPI
	}
	return out
}
