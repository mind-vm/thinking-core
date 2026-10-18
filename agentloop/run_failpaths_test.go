package agentloop_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mind-vm/thinking-core/agentloop"
	"github.com/mind-vm/thinking-core/llm"
	"github.com/mind-vm/thinking-core/sandbox"
)

// This file pins the failure-path branches of run.go. Each has a named
// test so the documented best-effort-vs-fatal semantics (see the
// package doc / README) stay true across changes.

// ---- additional test doubles ----

// alwaysJSClient is an LLM that never emits DONE — every turn is a JS
// fence. Drives the MaxIterations branch.
type alwaysJSClient struct{}

func (alwaysJSClient) Name() string { return "always-js" }

func (alwaysJSClient) Complete(_ context.Context, _ llm.CompletionRequest) (llm.CompletionResponse, error) {
	return llm.CompletionResponse{Content: "```js\n1\n```", InputTokens: 1, OutputTokens: 1}, nil
}

func (c alwaysJSClient) Stream(ctx context.Context, req llm.CompletionRequest, onChunk func(string)) (llm.CompletionResponse, error) {
	resp, err := c.Complete(ctx, req)
	if err == nil && onChunk != nil {
		onChunk(resp.Content)
	}
	return resp, err
}

// failBuilder fails to construct a sandbox. Drives the SandboxBuilder.Build
// failure branch.
type failBuilder struct{ err error }

func (b failBuilder) Build(_ context.Context, _ agentloop.Session, _ agentloop.Scope, _ sandbox.OnEvent) (*sandbox.Sandbox, func(), error) {
	return nil, nil, b.err
}

// eventWithContent reports whether any event of the given type carries
// content containing substr.
func eventWithContent(events []agentloop.RunEvent, typ, substr string) bool {
	for _, e := range events {
		if e.Type == typ && strings.Contains(e.Content, substr) {
			return true
		}
	}
	return false
}

// ---- construction: required deps panic at New (fail at boot, not request) ----

func TestNew_PanicsWhenRequiredDepMissing(t *testing.T) {
	full := func() agentloop.Config {
		return agentloop.Config{
			LLM:            newFakeLLM("fake"),
			Sessions:       newMemorySessions(),
			Steps:          newMemorySteps(),
			SandboxBuilder: emptyBuilder{},
		}
	}
	cases := map[string]func(*agentloop.Config){
		"missing LLM":            func(c *agentloop.Config) { c.LLM = nil },
		"missing Sessions":       func(c *agentloop.Config) { c.Sessions = nil },
		"missing Steps":          func(c *agentloop.Config) { c.Steps = nil },
		"missing SandboxBuilder": func(c *agentloop.Config) { c.SandboxBuilder = nil },
	}
	for name, drop := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := full()
			drop(&cfg)
			defer func() {
				if recover() == nil {
					t.Errorf("New should panic when a required dep is nil")
				}
			}()
			agentloop.New(cfg)
		})
	}
}

// ---- branch: MaxIterations exhausted without DONE ----

func TestRun_MaxIterations(t *testing.T) {
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM:            alwaysJSClient{},
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: emptyBuilder{},
	})

	var events []agentloop.RunEvent
	result, err := loop.Run(context.Background(), agentloop.RunRequest{
		SessionID: "s1", Message: "loop forever",
		OnEvent: func(e agentloop.RunEvent) { events = append(events, e) },
	})
	if err == nil {
		t.Fatalf("expected an error when max iterations is hit")
	}
	if result.Status != "max_iterations" {
		t.Fatalf("status: got %q, want %q", result.Status, "max_iterations")
	}
	if !contains(steps.types("s1"), "error") {
		t.Errorf("expected a terminal error step, got: %v", steps.types("s1"))
	}
	if !eventWithContent(events, "error", "max iterations") {
		t.Errorf("expected a max-iterations error event")
	}
	if fin := sessions.finalised("s1"); fin == nil || fin.Status != "max_iterations" {
		t.Errorf("session should be finalised with max_iterations, got %+v", fin)
	}
}

// ---- branch: context cancelled before/between turns ----

func TestRun_ContextCancelled(t *testing.T) {
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM:            newFakeLLM("fake", llm.CompletionResponse{Content: "DONE\nunreached"}),
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: emptyBuilder{},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Run — the loop's top-of-iteration guard catches it.

	result, err := loop.Run(ctx, agentloop.RunRequest{SessionID: "s1", Message: "hi"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if result.Status != "error" {
		t.Fatalf("status: got %q, want %q", result.Status, "error")
	}
	if fin := sessions.finalised("s1"); fin == nil || fin.Status != "error" {
		t.Errorf("session should be finalised error, got %+v", fin)
	}
}

// ---- branch: Sessions.Get fails → early return, fatal ----

func TestRun_SessionGetFails(t *testing.T) {
	sessions := newMemorySessions()
	sessions.getErr = errors.New("db down")
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM:            newFakeLLM("fake", llm.CompletionResponse{Content: "DONE\nx"}),
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: emptyBuilder{},
	})

	_, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi"})
	if err == nil || !strings.Contains(err.Error(), "load session") {
		t.Fatalf("expected a load-session error, got %v", err)
	}
	if len(steps.types("s1")) != 0 {
		t.Errorf("no steps should be written when session load fails, got %v", steps.types("s1"))
	}
	if sessions.finalised("s1") != nil {
		t.Errorf("session should not be finalised on early load failure")
	}
}

// ---- branch: Steps.LastN fails → best-effort, run continues ----

func TestRun_HistoryReadFails_ContinuesWithEmptyHistory(t *testing.T) {
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()
	steps.lastNErr = errors.New("flaky read")

	loop := agentloop.New(agentloop.Config{
		LLM:            newFakeLLM("fake", llm.CompletionResponse{Content: "DONE\nstill answered"}),
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: emptyBuilder{},
	})

	var events []agentloop.RunEvent
	result, err := loop.Run(context.Background(), agentloop.RunRequest{
		SessionID: "s1", Message: "hi",
		OnEvent: func(e agentloop.RunEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("history read failure must NOT abort the run, got %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("status: got %q, want %q", result.Status, "completed")
	}
	if !eventWithContent(events, "warning", "history rehydrate failed") {
		t.Errorf("expected a history-rehydrate warning event")
	}
}

// ---- branch: SandboxBuilder.Build fails → finalize(error), fatal ----

func TestRun_SandboxBuildFails(t *testing.T) {
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM:            newFakeLLM("fake", llm.CompletionResponse{Content: "DONE\nx"}),
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: failBuilder{err: errors.New("pack registry empty")},
	})

	result, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi"})
	if err == nil || !strings.Contains(err.Error(), "build sandbox") {
		t.Fatalf("expected a build-sandbox error, got %v", err)
	}
	if result.Status != "error" {
		t.Fatalf("status: got %q, want %q", result.Status, "error")
	}
	if fin := sessions.finalised("s1"); fin == nil || fin.Status != "error" {
		t.Errorf("session should be finalised error after build failure, got %+v", fin)
	}
}

// ---- branch: Steps.Append fails → best-effort warning, continues ----

func TestRun_StepAppendFails_WarnsAndContinues(t *testing.T) {
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()
	steps.appendErr = errors.New("write failed")

	loop := agentloop.New(agentloop.Config{
		LLM:            newFakeLLM("fake", llm.CompletionResponse{Content: "DONE\nanswer"}),
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: emptyBuilder{},
	})

	var events []agentloop.RunEvent
	result, err := loop.Run(context.Background(), agentloop.RunRequest{
		SessionID: "s1", Message: "hi",
		OnEvent: func(e agentloop.RunEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("step persistence failure must NOT abort, got %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("status: got %q, want %q", result.Status, "completed")
	}
	if !eventWithContent(events, "warning", "persist step failed") {
		t.Errorf("expected a persist-step warning event")
	}
}

// ---- branch: Sessions.Finalize fails → warning, result still returned ----

func TestRun_FinalizeFails_WarnsAndReturnsResult(t *testing.T) {
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	sessions.finalizeErr = errors.New("finalize write failed")
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM:            newFakeLLM("fake", llm.CompletionResponse{Content: "DONE\nthe answer"}),
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: emptyBuilder{},
	})

	var events []agentloop.RunEvent
	result, err := loop.Run(context.Background(), agentloop.RunRequest{
		SessionID: "s1", Message: "hi",
		OnEvent: func(e agentloop.RunEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("finalize failure must NOT abort, got %v", err)
	}
	if result.Status != "completed" || result.FinalText != "the answer" {
		t.Fatalf("result should still be returned: status=%q text=%q", result.Status, result.FinalText)
	}
	if !eventWithContent(events, "warning", "finalize failed") {
		t.Errorf("expected a finalize-failed warning event")
	}
}

// ---- branch: Sessions.UpdateData fails → warning, loop continues ----

func TestRun_UpdateDataFails_WarnsAndContinues(t *testing.T) {
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	sessions.updateDataErr = errors.New("snapshot write failed")
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM: newFakeLLM("fake",
			llm.CompletionResponse{Content: "```js\nfunction run(args) { return { x: 1 }; }\n```"},
			llm.CompletionResponse{Content: "DONE\ndone"},
		),
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: dataBuilder{},
	})

	var events []agentloop.RunEvent
	result, err := loop.Run(context.Background(), agentloop.RunRequest{
		SessionID: "s1", Message: "set x",
		OnEvent: func(e agentloop.RunEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("data snapshot failure must NOT abort, got %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("status: got %q, want %q", result.Status, "completed")
	}
	if !eventWithContent(events, "warning", "data snapshot persist failed") {
		t.Errorf("expected a data-snapshot warning event")
	}
}

// ---- branch: JS execution error fed back into history, loop continues ----

func TestRun_JSExecError_FedBackAndContinues(t *testing.T) {
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM: newFakeLLM("fake",
			llm.CompletionResponse{Content: "```js\nthrow new Error(\"boom\")\n```"},
			llm.CompletionResponse{Content: "DONE\nrecovered"},
		),
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: emptyBuilder{},
	})

	var events []agentloop.RunEvent
	result, err := loop.Run(context.Background(), agentloop.RunRequest{
		SessionID: "s1", Message: "do a bad thing",
		OnEvent: func(e agentloop.RunEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("a thrown JS error must NOT kill the run, got %v", err)
	}
	if result.Status != "completed" || result.FinalText != "recovered" {
		t.Fatalf("run should recover and complete: status=%q text=%q", result.Status, result.FinalText)
	}
	// The error surfaces as the execute_js_result content, fed back to the model.
	if !eventWithContent(events, "execute_js_result", "Error:") {
		t.Errorf("expected the JS error surfaced in execute_js_result")
	}
	want := []string{"user", "execute_js", "execute_js_result", "response"}
	if got := steps.types("s1"); !equalSlice(got, want) {
		t.Errorf("step types: got %v, want %v", got, want)
	}
}

// ---- branch: empty LLM completions are retried, then a typed error ----

func TestRun_EmptyResponse_RetriesThenSucceeds(t *testing.T) {
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM: newFakeLLM("fake",
			llm.CompletionResponse{Content: "  \n  "}, // whitespace-only counts as empty
			llm.CompletionResponse{Content: ""},
			llm.CompletionResponse{Content: "DONE\nfinally"},
		),
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: emptyBuilder{},
	})

	var events []agentloop.RunEvent
	result, err := loop.Run(context.Background(), agentloop.RunRequest{
		SessionID: "s1", Message: "hi",
		OnEvent: func(e agentloop.RunEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("run should recover from transient empty completions: %v", err)
	}
	if result.Status != "completed" || result.FinalText != "finally" {
		t.Fatalf("status=%q final=%q", result.Status, result.FinalText)
	}
	if !eventWithContent(events, "warning", "empty model turn") {
		t.Errorf("expected a warning event for each retried empty turn")
	}
}

// An empty reply is only diagnosable if the call that produced it is on
// the record: a "length" finish with thinking tokens and no content says
// the output budget went to reasoning, which a bare retry warning cannot.
func TestRun_EveryCallReportsThinkingAndFinishReason(t *testing.T) {
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})

	loop := agentloop.New(agentloop.Config{
		LLM: newFakeLLM("fake",
			llm.CompletionResponse{Reasoning: "let me think", FinishReason: "length", OutputTokens: 900, ReasoningTokens: 900},
			llm.CompletionResponse{Content: "DONE\nok", FinishReason: "stop", OutputTokens: 5},
		),
		Sessions:       sessions,
		Steps:          newMemorySteps(),
		SandboxBuilder: emptyBuilder{},
	})

	var calls []agentloop.RunEvent
	_, err := loop.Run(context.Background(), agentloop.RunRequest{
		SessionID: "s1", Message: "hi",
		OnEvent: func(e agentloop.RunEvent) {
			if e.Type == "llm_call" {
				calls = append(calls, e)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("got %d llm_call events, want one per call (2)", len(calls))
	}
	empty := calls[0]
	if empty.Content != "let me think" || empty.Args["finish_reason"] != "length" ||
		empty.Args["reasoning_tokens"] != 900 || empty.Args["content_bytes"] != 0 {
		t.Errorf("empty call reported %q %v", empty.Content, empty.Args)
	}
	if empty.Tokens == nil || empty.Tokens.Completion != 900 {
		t.Errorf("empty call tokens = %+v", empty.Tokens)
	}
	if calls[1].Args["finish_reason"] != "stop" {
		t.Errorf("second call reported %v", calls[1].Args)
	}
}

func TestRun_EmptyResponse_ExhaustsRetriesThenErrors(t *testing.T) {
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM: newFakeLLM("fake",
			llm.CompletionResponse{Content: ""},
			llm.CompletionResponse{Content: ""},
			llm.CompletionResponse{Content: ""},
		),
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: emptyBuilder{},
	})

	result, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi"})
	if !errors.Is(err, agentloop.ErrEmptyResponse) {
		t.Fatalf("expected ErrEmptyResponse, got %v", err)
	}
	if result.Status != "error" {
		t.Fatalf("status: got %q, want %q", result.Status, "error")
	}
	if !contains(steps.types("s1"), "error") {
		t.Errorf("expected a terminal error step, got: %v", steps.types("s1"))
	}
}

// ---- branch: token aggregation sums across turns ----

func TestRun_TokenAggregation_SumsAcrossTurns(t *testing.T) {
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM: newFakeLLM("fake",
			llm.CompletionResponse{Content: "```js\n1\n```", InputTokens: 4, OutputTokens: 2},
			llm.CompletionResponse{Content: "DONE\nfinished", InputTokens: 7, OutputTokens: 1},
		),
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: emptyBuilder{},
	})

	result, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "go"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got, want := result.Tokens.Prompt, int32(11); got != want {
		t.Errorf("summed prompt tokens: got %d, want %d", got, want)
	}
	if got, want := result.Tokens.Completion, int32(3); got != want {
		t.Errorf("summed completion tokens: got %d, want %d", got, want)
	}
}

// ---- branch: the run's own cap reached mid-call ----

// blockingClient answers nothing until its context is done, the way a
// slow host looks from inside one long call.
type blockingClient struct{}

func (blockingClient) Name() string { return "blocking" }
func (blockingClient) Complete(ctx context.Context, _ llm.CompletionRequest) (llm.CompletionResponse, error) {
	<-ctx.Done()
	return llm.CompletionResponse{}, ctx.Err()
}
func (b blockingClient) Stream(ctx context.Context, req llm.CompletionRequest, _ func(string)) (llm.CompletionResponse, error) {
	return b.Complete(ctx, req)
}

// A run cut off by Config.RunTimeout says so, rather than surfacing the
// call's "context deadline exceeded", which reads as a stalled host.
func TestRun_RunTimeoutIsNamed(t *testing.T) {
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	loop := agentloop.New(agentloop.Config{
		LLM: blockingClient{}, Sessions: sessions, Steps: newMemorySteps(),
		SandboxBuilder: emptyBuilder{}, RunTimeout: 50 * time.Millisecond,
	})
	var events []string
	_, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi", OnEvent: func(e agentloop.RunEvent) {
		if e.Type == "error" {
			events = append(events, e.Content)
		}
	}})
	if !errors.Is(err, agentloop.ErrRunTimeout) {
		t.Fatalf("err = %v, want ErrRunTimeout", err)
	}
	if !strings.Contains(err.Error(), "50ms") || len(events) != 1 || !strings.Contains(events[0], "run timeout") {
		t.Errorf("the cap is not named: err %q, events %q", err, events)
	}
}

// The host's own hint comes before the generic one: it knows its names.
func TestRun_HostRetryHintComesFirst(t *testing.T) {
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	loop := agentloop.New(agentloop.Config{
		LLM: newFakeLLM("fake",
			llm.CompletionResponse{Content: "```js\nopen.id\n```"},
			llm.CompletionResponse{Content: "DONE\nok"},
		),
		Sessions:       sessions,
		Steps:          newMemorySteps(),
		SandboxBuilder: emptyBuilder{},
		RetryHint: func(msg string) string {
			if strings.Contains(msg, "open is not defined") {
				return "open is $.open"
			}
			return ""
		},
	})
	var events []agentloop.RunEvent
	if _, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "x", OnEvent: func(e agentloop.RunEvent) { events = append(events, e) }}); err != nil {
		t.Fatal(err)
	}
	if !eventWithContent(events, "execute_js_result", "RETRY HINT: open is $.open") {
		t.Error("the host's hint was not used")
	}
}

// An error the host names final reaches the model as it is: no hint and no
// "fix and retry" footer to contradict its "do not retry". Others keep both.
func TestRun_FinalErrorIsNotTurnedIntoARetry(t *testing.T) {
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	loop := agentloop.New(agentloop.Config{
		LLM: newFakeLLM("fake",
			llm.CompletionResponse{Content: "```js\nthrow new Error(\"declined; do not retry it\")\n```"},
			llm.CompletionResponse{Content: "```js\nopen.id\n```"},
			llm.CompletionResponse{Content: "DONE\nok"},
		),
		Sessions:       sessions,
		Steps:          newMemorySteps(),
		SandboxBuilder: emptyBuilder{},
		FinalError:     func(msg string) bool { return strings.Contains(msg, "do not retry") },
	})
	var events []agentloop.RunEvent
	if _, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "x", OnEvent: func(e agentloop.RunEvent) { events = append(events, e) }}); err != nil {
		t.Fatal(err)
	}
	var results []string
	for _, e := range events {
		if e.Type == "execute_js_result" {
			results = append(results, e.Content)
		}
	}
	if len(results) != 2 {
		t.Fatalf("want two results, got %q", results)
	}
	if !strings.Contains(results[0], "do not retry it") || strings.Contains(results[0], "fix and retry") || strings.Contains(results[0], "RETRY HINT") {
		t.Errorf("the final error was dressed as a retry: %q", results[0])
	}
	if !strings.Contains(results[1], "fix and retry") {
		t.Errorf("an ordinary error lost its retry footer: %q", results[1])
	}
}
