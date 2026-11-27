package reliability

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mind-vm/thinking-core/agentloop"
	"github.com/mind-vm/thinking-core/sandbox"
)

// An arm is one way of letting the model act. A–D are code arms that differ
// only in how data reaches the next script; E is native tool calling; L is
// arm C run through the real agentloop, to show what the library's guards
// add on top of the bare mechanism.
const (
	ArmScript = "A-script" // a plain script, nothing carries over
	ArmMain   = "B-main"   // function main(state) { …; return state }
	ArmState  = "C-state"  // a plain script, the global state carries (today's library)
	ArmData   = "D-data"   // a global data object whose values are shown each turn
	ArmTools  = "E-tools"  // native tool calls
	ArmLoop   = "L-loop"   // C through agentloop
)

var AllArms = []string{ArmScript, ArmMain, ArmState, ArmData, ArmTools, ArmLoop}

// Limits shared by every arm. MaxTurns counts model calls per user message.
const (
	MaxTurns    = 8
	RunTimeout  = 4 * time.Minute
	ScriptLimit = 10 * time.Second
	// DataShown caps arm D's state dump and E's tool results, so one huge
	// value cannot sink a run on its own; both say when they cut.
	DataShown = 64 * 1024
)

// Result is one run, one line of results.jsonl.
type Result struct {
	Arm   string `json:"arm"`
	Model string `json:"model"`
	Task  string `json:"task"`
	Rep   int    `json:"rep"`

	Pass    bool   `json:"pass"`
	Expect  string `json:"expect"`
	Failure string `json:"failure,omitempty"` // a class from classify, when not passed

	// Status is how the run ended: "answered" (answer() or a final text),
	// "turn_cap", "llm_error" or "timeout".
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`

	Answers []string `json:"answers"`
	Calls   []Call   `json:"calls"`

	Turns            int     `json:"turns"` // model calls, all messages
	ScriptErrors     int     `json:"script_errors"`
	FirstValid       bool    `json:"first_valid"` // the first reply ran (or called a tool) without an error
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	ReasoningTokens  int     `json:"reasoning_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	DurationMs       int64   `json:"duration_ms"`

	Transcript []Msg `json:"-"`
}

func (r *Result) add(m Model, rep Reply) {
	r.Turns++
	r.PromptTokens += rep.PromptTokens
	r.CompletionTokens += rep.CompletionTokens
	r.ReasoningTokens += rep.ReasoningTokens
	r.CostUSD = m.Cost(r.PromptTokens, r.CompletionTokens)
}

const protocolHead = "You solve tasks by writing JavaScript. Reply with exactly one ```javascript block and nothing else, then stop: it runs in a sandbox, and what it logs comes back to you as the next message. Continue like that until you call answer(result).\n\n"

const protocolTail = "- log() what you need to read yourself: counts, names, small samples, not whole datasets.\n" +
	"- The runtime is synchronous: no await or Promises, no Node or browser APIs. The functions declared below are all there is."

// carryLine is the one line of the protocol that differs between code arms.
// C's is the library's own (agentloop.TextEmissionSystemPrompt).
var carryLine = map[string]string{
	ArmScript: "- Each script runs fresh: nothing carries over to the next script. What you need again, fetch again or read from your earlier output.\n",
	ArmMain:   "- Each reply defines function main(state) { … return state; }. It is called with what the previous main returned ({} the first time), and only what it returns carries over, saved with JSON.stringify. You see its shape, not its values.\n",
	ArmState:  "- Each script runs fresh; only `state` carries over, saved with JSON.stringify. You see its shape, not its values.\n",
	ArmData:   "- Each script runs fresh; only the global `data` carries over, saved with JSON.stringify. Its current value is shown to you before each script.\n",
}

const answerLogDecl = "/** End the task and give result (markdown or JSON) to the user. */\ndeclare function answer(result: string | object): void;\n" +
	"/** Show values to yourself; you read them before writing the next script. */\ndeclare function log(...values: any[]): void;"

var carryDecl = map[string]string{
	ArmScript: "",
	ArmMain:   "\n/** You define it in every reply. It gets what the previous main returned ({} the first time); what it returns is kept for the next one. */\ndeclare function main(state: Record<string, any>): Record<string, any>;",
	ArmState:  "\n/** Persists between your scripts. You see its shape, not its values. */\ndeclare const state: Record<string, any>;",
	ArmData:   "\n/** Persists between your scripts. Its current value is shown to you before each script. */\ndeclare let data: Record<string, any>;",
}

func codeSystemPrompt(arm string, sb *sandbox.Sandbox) string {
	return protocolHead + carryLine[arm] + protocolTail + "\n\n## Sandbox API\n\n" + sb.SystemPrompt()
}

// wrap adapts the model's script to its arm. Every arm runs through
// ExecuteTurn, which injects and reads back the global state; the wrapping
// only decides what that global means.
func wrap(arm, code string) string {
	switch arm {
	case ArmMain:
		// A script without main is run as it stands, so state assigned at
		// the top level still carries: the arm's prompt asks for main, and
		// noMain counts how often a model ignores that.
		return code + "\n;if (typeof main === 'function') { const __next = main(state); state = (__next === undefined || __next === null) ? {} : __next; }"
	case ArmData:
		// On the script's first line, so error line numbers stay right.
		return "globalThis.data = state; " + code + "\n;state = globalThis.data;"
	}
	return code
}

// stateView is the ephemeral message shown before each script: the shape of
// what carried (B, C), its values (D), or nothing (A).
func stateView(arm string, state json.RawMessage) string {
	switch arm {
	case ArmMain:
		if s := dataShape(state); s != "" {
			return "what main returned last (shape only):\n" + s
		}
	case ArmState:
		if s := dataShape(state); s != "" {
			return "state (shape only):\n" + s
		}
	case ArmData:
		if len(state) > 0 && string(state) != "{}" && string(state) != "null" {
			s := string(state)
			if len(s) > DataShown {
				s = s[:DataShown] + fmt.Sprintf("… (cut, %d bytes in all)", len(state))
			}
			return "data (current value):\n" + s
		}
	}
	return ""
}

// RunCode drives arms A–D.
func RunCode(ctx context.Context, c *Chat, m Model, arm string, t Task) Result {
	r := Result{Arm: arm, Model: m.Name, Task: t.ID}
	ctx, cancel := context.WithTimeout(ctx, RunTimeout)
	defer cancel()
	start := time.Now()
	defer func() { r.DurationMs = time.Since(start).Milliseconds() }()

	w := NewWorld(t.Functions)
	sb := sandbox.New(Pack(w, t.Functions))
	sb.SetExecTimeout(ScriptLimit)
	sb.SetCorePrompt(answerLogDecl + carryDecl[arm])
	history := []Msg{{Role: "system", Content: codeSystemPrompt(arm, sb)}}
	state := json.RawMessage("{}")
	first := true

	for _, msg := range t.Messages {
		history = append(history, Msg{Role: "user", Content: msg})
		answer, status, err := "", "turn_cap", error(nil)
		for turn := 0; turn < MaxTurns; turn++ {
			call := history
			if v := stateView(arm, state); v != "" {
				call = append(append([]Msg{}, history...), Msg{Role: "user", Content: v})
			}
			rep, cerr := c.Complete(ctx, m, call, nil)
			if cerr != nil {
				status, err = "llm_error", cerr
				if ctx.Err() != nil {
					status = "timeout"
				}
				break
			}
			r.add(m, rep)
			text := rep.Content
			if strings.TrimSpace(text) == "" {
				// An empty reply ends nothing: it is a turn that did nothing,
				// said back to the model the way the library says it.
				history = append(history, Msg{Role: "assistant", Content: "(empty reply)"}, Msg{Role: "user", Content: "Your reply was empty. Reply with exactly one ```javascript block."})
				first = false
				continue
			}
			history = append(history, Msg{Role: "assistant", Content: text})
			code := agentloop.ExtractJSBlock(text)
			if code == "" {
				// No script is a final answer, as in the library.
				answer, status = strings.TrimSpace(text), "answered"
				if first {
					r.FirstValid = true
				}
				break
			}
			in := state
			if arm == ArmScript {
				in = json.RawMessage("{}")
			}
			logs, ret, xerr := sb.ExecuteTurn(wrap(arm, code), in)
			out := "Execution result:\n"
			if xerr != nil {
				r.ScriptErrors++
				if logs != "" {
					out += logs + "\n"
				}
				out += "Error: " + cleanError(xerr)
				if arm != ArmScript {
					out += "\n(state is unchanged — fix and retry.)"
				}
			} else {
				if first {
					r.FirstValid = true
				}
				if arm != ArmScript && ret != nil {
					state = ret
				}
				if v, ok := sb.TakeAnswer(); ok {
					answer, status = v, "answered"
					first = false
					break
				}
				if logs == "" {
					logs = "(no output)"
				}
				out += logs
			}
			first = false
			history = append(history, Msg{Role: "user", Content: out})
		}
		r.Answers = append(r.Answers, answer)
		r.Status = status
		if err != nil {
			r.Error = err.Error()
		}
		if status != "answered" {
			break
		}
	}
	r.Calls, r.Transcript = w.Calls(), history
	finish(&r, t, w)
	return r
}

// cleanError drops goja's stack frames, keeping the message a model can act on.
func cleanError(err error) string {
	var keep []string
	for _, l := range strings.Split(err.Error(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "at ") {
			continue
		}
		keep = append(keep, l)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}

const toolSystemPrompt = "You solve tasks with the functions provided as tools. Call them as needed; when you have the result, reply with your final answer as plain text, without a tool call."

// RunTools drives arm E: native tool calls over the same functions.
func RunTools(ctx context.Context, c *Chat, m Model, t Task) Result {
	r := Result{Arm: ArmTools, Model: m.Name, Task: t.ID}
	ctx, cancel := context.WithTimeout(ctx, RunTimeout)
	defer cancel()
	start := time.Now()
	defer func() { r.DurationMs = time.Since(start).Milliseconds() }()

	w := NewWorld(t.Functions)
	tools := Tools(t.Functions)
	history := []Msg{{Role: "system", Content: toolSystemPrompt}}
	first := true

	for _, msg := range t.Messages {
		history = append(history, Msg{Role: "user", Content: msg})
		answer, status, err := "", "turn_cap", error(nil)
		for turn := 0; turn < MaxTurns; turn++ {
			rep, cerr := c.Complete(ctx, m, history, tools)
			if cerr != nil {
				status, err = "llm_error", cerr
				if ctx.Err() != nil {
					status = "timeout"
				}
				break
			}
			r.add(m, rep)
			if len(rep.ToolCalls) == 0 {
				if strings.TrimSpace(rep.Content) == "" {
					history = append(history, Msg{Role: "assistant", Content: "(empty reply)"}, Msg{Role: "user", Content: "Your reply was empty. Call a tool, or reply with your final answer as text."})
					first = false
					continue
				}
				if first {
					r.FirstValid = true
				}
				answer, status = strings.TrimSpace(rep.Content), "answered"
				history = append(history, Msg{Role: "assistant", Content: rep.Content})
				break
			}
			var content any
			if rep.Content != "" {
				content = rep.Content
			}
			history = append(history, Msg{Role: "assistant", Content: content, ToolCalls: rep.ToolCalls})
			ok := true
			for _, tc := range rep.ToolCalls {
				history = append(history, Msg{Role: "tool", ToolCallID: tc.ID, Content: toolResult(w, tc, &ok)})
			}
			if !ok {
				r.ScriptErrors++
			}
			if first && ok {
				r.FirstValid = true
			}
			first = false
		}
		r.Answers = append(r.Answers, answer)
		r.Status = status
		if err != nil {
			r.Error = err.Error()
		}
		if status != "answered" {
			break
		}
	}
	r.Calls, r.Transcript = w.Calls(), history
	finish(&r, t, w)
	return r
}

func toolResult(w *World, tc ToolCall, ok *bool) string {
	args := map[string]any{}
	if s := strings.TrimSpace(tc.Function.Arguments); s != "" && s != "null" {
		if err := json.Unmarshal([]byte(s), &args); err != nil {
			*ok = false
			w.record(Call{Name: tc.Function.Name, Error: "arguments are not valid JSON"})
			return `{"error":"arguments are not valid JSON: ` + jsonEscape(err.Error()) + `"}`
		}
	}
	res, err := w.Invoke(tc.Function.Name, args)
	if err != nil {
		*ok = false
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(b)
	}
	b, _ := json.Marshal(res)
	if len(b) > DataShown {
		return string(b[:DataShown]) + fmt.Sprintf("… (cut, %d bytes in all)", len(b))
	}
	return string(b)
}

func jsonEscape(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}

// finish checks the outcome and, when the run failed, names why.
func finish(r *Result, t Task, w *World) {
	o := &Outcome{Answers: r.Answers, Calls: r.Calls, Tickets: w.Tickets()}
	r.Pass, r.Expect = t.Check(o)
	if !r.Pass {
		r.Failure = classify(r, t, o)
	}
}

// classify names the first thing that went wrong, in the order a reader
// would look for it: the run did not finish, then the protocol broke, then
// the effects were wrong, then the answer was.
func classify(r *Result, t Task, o *Outcome) string {
	switch r.Status {
	case "llm_error", "timeout":
		return r.Status
	case "turn_cap":
		return "turn_cap"
	}
	if t.ID == "04-carry" {
		for _, c := range o.Calls {
			if strings.Contains(c.Error, "quota exceeded") {
				return "lost_data"
			}
		}
	}
	for _, c := range o.Calls {
		if strings.HasPrefix(c.Error, "unknown function") {
			return "unknown_function"
		}
	}
	if name, called := undefinedName(r.Transcript); name != "" {
		// A name called like a function was invented; any other undefined
		// name is a variable an earlier script had and this one does not.
		if called {
			return "unknown_function"
		}
		return "lost_variable"
	}
	switch t.ID {
	case "08-notool":
		if len(o.Calls) > 0 {
			return "unneeded_calls"
		}
	case "06-args", "10-once":
		if len(o.Tickets) > 1 {
			return "duplicate_effect"
		}
	}
	for _, c := range o.Calls {
		if c.Error != "" && !strings.Contains(c.Error, "(503)") {
			return "bad_args"
		}
	}
	if strings.TrimSpace(o.last()) == "" {
		return "no_answer"
	}
	return "wrong_answer"
}

var undefinedRef = regexp.MustCompile(`ReferenceError: ([\pL_$][\pL\pN_$]*) is not defined`)

// undefinedName finds the first ReferenceError in the run and whether the
// script before it called that name as a function.
func undefinedName(ms []Msg) (string, bool) {
	for i, m := range ms {
		c, ok := m.Content.(string)
		if !ok || m.Role != "user" || !strings.HasPrefix(c, "Execution result:") {
			continue
		}
		sub := undefinedRef.FindStringSubmatch(c)
		if sub == nil {
			continue
		}
		code := ""
		if i > 0 {
			code, _ = ms[i-1].Content.(string)
		}
		return sub[1], regexp.MustCompile(`\b` + regexp.QuoteMeta(sub[1]) + `\s*\(`).MatchString(code)
	}
	return "", false
}
