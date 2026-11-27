package reliability

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mind-vm/thinking-core/agentloop"
	"github.com/mind-vm/thinking-core/agentloopmem"
	"github.com/mind-vm/thinking-core/llm"
	"github.com/mind-vm/thinking-core/sandbox"
)

// RunLoop drives arm L: the same task through the library's agentloop, with
// its default protocol prompt, client, retries and guards. Against arm C it
// measures what the library adds to the bare mechanism.
func RunLoop(ctx context.Context, baseURL, apiKey string, m Model, t Task) Result {
	r := Result{Arm: ArmLoop, Model: m.Name, Task: t.ID}
	start := time.Now()
	defer func() { r.DurationMs = time.Since(start).Milliseconds() }()

	client, err := llm.NewOpenAI(llm.Config{APIKey: apiKey, BaseURL: baseURL, ChatModel: m.ID, ReasoningEffort: m.ReasoningEffort, MaxRetries: 3})
	if err != nil {
		r.Status, r.Error = "llm_error", err.Error()
		return r
	}
	w := NewWorld(t.Functions)
	l := agentloop.New(agentloop.Config{
		LLM:      client,
		Sessions: agentloopmem.NewSessionStore(nil),
		Steps:    agentloopmem.NewStepStore(),
		SandboxBuilder: &agentloop.DefaultSandboxBuilder{
			NoDiscovery: true,
			Capabilities: []agentloop.Capability{{
				Name: "shop",
				Build: func(agentloop.BuildContext) ([]sandbox.Pack, error) {
					return []sandbox.Pack{Pack(w, t.Functions)}, nil
				},
			}},
		},
		MaxIterations: MaxTurns,
		RunTimeout:    RunTimeout,
	})

	session := fmt.Sprintf("bench-%s-%d", t.ID, time.Now().UnixNano())
	first := true
	for _, msg := range t.Messages {
		var transcript []Msg
		res, err := l.Run(ctx, agentloop.RunRequest{
			SessionID: session,
			Message:   msg,
			OnEvent: func(e agentloop.RunEvent) {
				switch e.Type {
				case "llm_call":
					r.Turns++
					if e.Tokens != nil {
						r.PromptTokens += int(e.Tokens.Prompt)
						r.CompletionTokens += int(e.Tokens.Completion)
					}
					if n, ok := e.Args["reasoning_tokens"].(int); ok {
						r.ReasoningTokens += n
					}
				case "execute_js":
					transcript = append(transcript, Msg{Role: "assistant", Content: e.Content})
				case "execute_js_result":
					failed := strings.HasPrefix(e.Content, "Error:")
					if failed {
						r.ScriptErrors++
					}
					if first && !failed {
						r.FirstValid = true
					}
					first = false
					transcript = append(transcript, Msg{Role: "user", Content: "Execution result:\n" + e.Content})
				case "response":
					if first {
						r.FirstValid = true
					}
					first = false
				}
			},
		})
		r.Transcript = append(r.Transcript, transcript...)
		r.CostUSD = m.Cost(r.PromptTokens, r.CompletionTokens)
		switch {
		case err == nil && res.Status == "completed":
			r.Status = "answered"
			r.Answers = append(r.Answers, strings.TrimSpace(res.FinalText))
		case res.Status == "max_iterations":
			r.Status = "turn_cap"
			r.Answers = append(r.Answers, "")
		default:
			r.Status = "llm_error"
			if errorsIsTimeout(err) {
				r.Status = "timeout"
			}
			if err != nil {
				r.Error = err.Error()
			}
			r.Answers = append(r.Answers, "")
		}
		if r.Status != "answered" {
			break
		}
	}
	r.Calls = w.Calls()
	finish(&r, t, w)
	return r
}

func errorsIsTimeout(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "timeout") || strings.Contains(err.Error(), "deadline"))
}
