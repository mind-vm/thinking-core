package agentloop_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dop251/goja"
	"strings"
	"sync"
	"testing"

	"github.com/mind-vm/agentloop"
	"github.com/mind-vm/agentloop/llm"
	"github.com/mind-vm/agentloop/sandbox"
)

// TestRun_DoneOnFirstTurn drives the simplest happy path: the LLM
// emits DONE + a final answer in one shot. Verifies the loop persists
// user + response steps, emits the lifecycle events, and finalizes
// completed.
func TestRun_DoneOnFirstTurn(t *testing.T) {
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: "DONE\nHere is the answer.", Model: "m", InputTokens: 5, OutputTokens: 3},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM:            fake,
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: emptyBuilder{},
	})

	var events []agentloop.RunEvent
	result, err := loop.Run(context.Background(), agentloop.RunRequest{
		SessionID: "s1",
		Message:   "hi",
		OnEvent:   func(e agentloop.RunEvent) { events = append(events, e) },
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("status: got %q, want %q", result.Status, "completed")
	}
	if result.FinalText != "Here is the answer." {
		t.Fatalf("final text: got %q, want %q", result.FinalText, "Here is the answer.")
	}
	if got, want := result.Tokens.Prompt, int32(5); got != want {
		t.Errorf("prompt tokens: got %d, want %d", got, want)
	}
	if got, want := result.Tokens.Completion, int32(3); got != want {
		t.Errorf("completion tokens: got %d, want %d", got, want)
	}

	gotTypes := eventTypes(events)
	for _, want := range []string{"user", "response", "done"} {
		if !contains(gotTypes, want) {
			t.Errorf("missing expected event %q (saw: %v)", want, gotTypes)
		}
	}

	wantStepTypes := []string{"user", "response"}
	gotStepTypes := steps.types("s1")
	if !equalSlice(gotStepTypes, wantStepTypes) {
		t.Errorf("persisted steps: got %v, want %v", gotStepTypes, wantStepTypes)
	}

	finalised := sessions.finalised("s1")
	if finalised == nil {
		t.Fatalf("session should be finalised")
	}
	if finalised.Status != "completed" {
		t.Errorf("finalised status: got %q, want %q", finalised.Status, "completed")
	}
}

// TestRun_TwoTurns_JSThenDone drives the canonical pattern: turn 1
// emits JS, the sandbox runs it, turn 2 emits DONE. Verifies the
// execute_js / execute_js_result steps are persisted with order, the
// data snapshot is captured, and the response carries the correct
// final text.
func TestRun_TwoTurns_JSThenDone(t *testing.T) {
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: "```javascript\nfunction run(args) { return { x: 1 }; }\n```", InputTokens: 4, OutputTokens: 2},
		llm.CompletionResponse{Content: "DONE\nDid it.", InputTokens: 7, OutputTokens: 1},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM:            fake,
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: dataBuilder{},
	})

	result, err := loop.Run(context.Background(), agentloop.RunRequest{
		SessionID: "s1",
		Message:   "set x to 1",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("status: got %q, want %q", result.Status, "completed")
	}
	if result.FinalText != "Did it." {
		t.Errorf("final text: got %q, want %q", result.FinalText, "Did it.")
	}

	gotStepTypes := steps.types("s1")
	want := []string{"user", "execute_js", "execute_js_result", "response"}
	if !equalSlice(gotStepTypes, want) {
		t.Fatalf("step types: got %v, want %v", gotStepTypes, want)
	}

	snap := sessions.lastSnapshot("s1")
	if !strings.Contains(string(snap), `"x":1`) {
		t.Errorf("data snapshot should persist x=1, got %s", snap)
	}
}

// TestRun_AnswerTerminatesRun confirms that calling answer() inside a
// script ends the run with that value as the final response — and that
// the loop does NOT make a further LLM call (the fake has only one reply,
// so a second call would error).
func TestRun_AnswerTerminatesRun(t *testing.T) {
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: "```javascript\nanswer(\"The result is 42.\")\n```", InputTokens: 4, OutputTokens: 2},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM:            fake,
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: dataBuilder{},
	})

	result, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "compute"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("status: got %q, want %q", result.Status, "completed")
	}
	if result.FinalText != "The result is 42." {
		t.Errorf("final text: got %q, want %q", result.FinalText, "The result is 42.")
	}
	gotStepTypes := steps.types("s1")
	want := []string{"user", "execute_js", "execute_js_result", "response"}
	if !equalSlice(gotStepTypes, want) {
		t.Fatalf("step types: got %v, want %v", gotStepTypes, want)
	}
}

// TestRun_AnswerJSON confirms answer() with a non-string value returns
// the JSON encoding as the final response.
func TestRun_AnswerJSON(t *testing.T) {
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: "```javascript\nanswer({ over: [\"Acme\"], count: 1 })\n```"},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM:            fake,
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: dataBuilder{},
	})

	result, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "who is over?"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("status: got %q", result.Status)
	}
	if !strings.Contains(result.FinalText, `"count":1`) || !strings.Contains(result.FinalText, `"over":["Acme"]`) {
		t.Errorf("final text should be the JSON answer, got %q", result.FinalText)
	}
}

// TestRun_NoFenceNoMarker_FreeformAsFinal confirms an LLM reply that's
// neither fenced JS nor DONE-prefixed is treated as the final answer
// rather than burning iterations.
func TestRun_NoFenceNoMarker_FreeformAsFinal(t *testing.T) {
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: "Plain answer, no markers.", InputTokens: 1, OutputTokens: 1},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM:            fake,
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: emptyBuilder{},
	})

	result, err := loop.Run(context.Background(), agentloop.RunRequest{
		SessionID: "s1",
		Message:   "hi",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Status != "completed" {
		t.Fatalf("status: got %q, want %q", result.Status, "completed")
	}
	if result.FinalText != "Plain answer, no markers." {
		t.Errorf("final text: got %q", result.FinalText)
	}
}

// TestRun_RequireScript_CorrectsProse confirms a RequireScript loop sends
// a scriptless reply back as a correction, and fails once the corrections
// run out: a reply that ran nothing is not a completed run.
func TestRun_RequireScript_CorrectsProse(t *testing.T) {
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: "Please provide the package."},
		llm.CompletionResponse{Content: "I would search for it."},
		llm.CompletionResponse{Content: "Still prose."},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})

	loop := agentloop.New(agentloop.Config{
		LLM:            fake,
		Sessions:       sessions,
		Steps:          newMemorySteps(),
		SandboxBuilder: emptyBuilder{},
		RequireScript:  true,
	})

	result, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi"})
	if !errors.Is(err, agentloop.ErrNoScript) {
		t.Fatalf("run err = %v, want ErrNoScript", err)
	}
	if got := len(fake.Calls()); got != 3 {
		t.Fatalf("llm calls: got %d, want 3", got)
	}
	second := fake.Calls()[1].Messages
	if last := second[len(second)-1].Content; !strings.Contains(last, "no ```javascript block") {
		t.Errorf("second call should end with the correction, got %q", last)
	}
	if result.Status != "error" || result.FinalText == "Still prose." {
		t.Errorf("result: got %q %q", result.Status, result.FinalText)
	}
}

// TestRun_RequireScript_NamesTheToolCall: a model that replied in its
// native tool-call format believes it acted, so the correction names the
// function it tried to call and says where that call belongs.
func TestRun_RequireScript_NamesTheToolCall(t *testing.T) {
	for _, reply := range []string{
		"<tool_call>\n<function=readFile>\n('inventory.go')\n</function>\n</tool_call>",
		"<tool_call>\n{\"name\": \"readFile\", \"arguments\": {\"path\": \"inventory.go\"}}\n</tool_call>",
		`{"name": "readFile", "arguments": {"path": "inventory.go"}}`,
		"<tool_call>\nreadFile inventory.go\n</tool_call>",
	} {
		fake := newFakeLLM("fake",
			llm.CompletionResponse{Content: reply},
			llm.CompletionResponse{Content: "DONE\nok"},
		)
		sessions := newMemorySessions()
		sessions.put("s1", agentloop.Session{ID: "s1"})
		loop := agentloop.New(agentloop.Config{
			LLM: fake, Sessions: sessions, Steps: newMemorySteps(),
			SandboxBuilder: emptyBuilder{}, RequireScript: true,
		})
		if _, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi"}); err != nil {
			t.Fatalf("run: %v", err)
		}
		msgs := fake.Calls()[1].Messages
		last := msgs[len(msgs)-1].Content
		named := strings.Contains(last, "tool call to `readFile`") || (!strings.Contains(reply, "<function=") && !strings.Contains(reply, `"name"`) && strings.Contains(last, "tool call to the function"))
		if !named || !strings.Contains(last, "inside one ```javascript block") || !strings.Contains(last, "answer(result)") {
			t.Errorf("correction for %q = %q", reply, last)
		}
	}
}

// TestRun_RequireScript_CorrectsACopiedPlaceholder: a reply that copies
// the elided-script marker is not run as a no-op turn; the model is told
// what it copied and asked for the next script.
func TestRun_RequireScript_CorrectsACopiedPlaceholder(t *testing.T) {
	copied := "```javascript\nfunction run(args) {\n  // earlier turn, body omitted: its logs are in the next message and its return value is in args\n  return args;\n}\n```"
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: copied},
		llm.CompletionResponse{Content: "DONE\nok"},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	loop := agentloop.New(agentloop.Config{
		LLM: fake, Sessions: sessions, Steps: newMemorySteps(),
		SandboxBuilder: emptyBuilder{}, RequireScript: true,
	})

	ran := false
	_, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi", OnEvent: func(e agentloop.RunEvent) {
		if e.Type == "execute_js" {
			ran = true
		}
	}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if ran {
		t.Error("the copied placeholder was executed")
	}
	msgs := fake.Calls()[1].Messages
	if last := msgs[len(msgs)-1].Content; !strings.Contains(last, "repeated the placeholder") {
		t.Errorf("second call should end with the placeholder correction, got %q", last)
	}
}

// TestRun_LLMError_SurfacedAsErrorStep confirms an LLM call error
// becomes an "error" step + RunResult.Status="error".
func TestRun_LLMError_SurfacedAsErrorStep(t *testing.T) {
	fake := newFakeLLM("fake") // no responses → first call errors
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM:            fake,
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: emptyBuilder{},
	})

	_, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi"})
	if err == nil {
		t.Fatalf("expected error from exhausted fake")
	}
	gotStepTypes := steps.types("s1")
	if !contains(gotStepTypes, "error") {
		t.Errorf("expected error step persisted, got: %v", gotStepTypes)
	}
}

// TestRun_MultiTurnHistory_FedBackToLLM confirms steps from a prior
// Run are replayed into the LLM's context on the next Run.
func TestRun_MultiTurnHistory_FedBackToLLM(t *testing.T) {
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: "DONE\nFirst answer."},
		llm.CompletionResponse{Content: "DONE\nSecond answer."},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM:            fake,
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: emptyBuilder{},
	})

	if _, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "first"}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "second"}); err != nil {
		t.Fatalf("second run: %v", err)
	}

	calls := fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", len(calls))
	}
	// Second call's messages include the first turn (user + response)
	// plus the new user turn.
	roles := []string{}
	for _, m := range calls[1].Messages {
		roles = append(roles, m.Role)
	}
	// system + user(first) + assistant(answer) + user(second)
	want := []string{"system", "user", "assistant", "user"}
	if !equalSlice(roles, want) {
		t.Fatalf("second call message roles: got %v, want %v", roles, want)
	}
}

// TestRun_InjectsContextIntoSystemPromptOnly verifies RunRequest.Context
// is folded into the system prompt for the LLM call but NOT persisted as
// the user turn (the trace + multi-turn history keep the raw Message).
func TestRun_InjectsContextIntoSystemPromptOnly(t *testing.T) {
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: "DONE\nok", Model: "m", InputTokens: 1, OutputTokens: 1},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1", SystemPrompt: "BASE-PROMPT"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM:            fake,
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: emptyBuilder{},
	})

	var userEventContent string
	_, err := loop.Run(context.Background(), agentloop.RunRequest{
		SessionID: "s1",
		Message:   "hi",
		Context:   "INJECTED-CTX-XYZ",
		OnEvent: func(e agentloop.RunEvent) {
			if e.Type == "user" {
				userEventContent = e.Content
			}
		},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	calls := fake.Calls()
	if len(calls) == 0 {
		t.Fatal("no llm calls recorded")
	}
	sys := calls[0].Messages[0]
	if sys.Role != "system" {
		t.Fatalf("first message role: got %q, want system", sys.Role)
	}
	if !strings.Contains(sys.Content, "INJECTED-CTX-XYZ") {
		t.Errorf("system prompt missing injected context: %q", sys.Content)
	}
	if !strings.Contains(sys.Content, "BASE-PROMPT") {
		t.Errorf("system prompt dropped the base prompt: %q", sys.Content)
	}
	// The user turn the loop records/emits must be the raw message.
	if userEventContent != "hi" {
		t.Errorf("user event content: got %q, want %q (context must not pollute the turn)", userEventContent, "hi")
	}
	for _, m := range calls[0].Messages {
		if m.Role == "user" && strings.Contains(m.Content, "INJECTED-CTX-XYZ") {
			t.Errorf("injected context leaked into the user message: %q", m.Content)
		}
	}
}

// TestRun_ProtocolReplacesTheDefaultContract: a Config.Protocol heads
// the system prompt in place of TextEmissionSystemPrompt, and the
// persona still follows it.
func TestRun_ProtocolReplacesTheDefaultContract(t *testing.T) {
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: "DONE\nok", Model: "m", InputTokens: 1, OutputTokens: 1},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1", SystemPrompt: "BASE-PROMPT"})

	loop := agentloop.New(agentloop.Config{
		LLM:            fake,
		Sessions:       sessions,
		Steps:          newMemorySteps(),
		SandboxBuilder: emptyBuilder{},
		Protocol:       "SHORT-PROTOCOL",
	})
	if _, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi"}); err != nil {
		t.Fatalf("run: %v", err)
	}

	sys := fake.Calls()[0].Messages[0].Content
	if !strings.HasPrefix(sys, "SHORT-PROTOCOL") {
		t.Errorf("system prompt does not open with the protocol: %q", sys)
	}
	if strings.Contains(sys, "Thinking Core") {
		t.Errorf("default contract still present: %q", sys)
	}
	if !strings.Contains(sys, "BASE-PROMPT") {
		t.Errorf("persona dropped: %q", sys)
	}
}

// TestRun_DataBytesCarried measures the context economy: a turn
// returns a large object; the next LLM call receives only the bounded
// shape digest while the full state stays server-side. The run's
// summary reports exactly the withheld delta.
func TestRun_DataBytesCarried(t *testing.T) {
	// A payload big enough that the digest is obviously smaller.
	big := `function run(args) { var blob = ''; for (var i = 0; i < 200; i++) blob += 'record-' + i + ';'; return { blob: blob, count: 200 }; }`
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: "```javascript\n" + big + "\n```"},
		llm.CompletionResponse{Content: "DONE\nStored."},
	)
	sessions := newMemorySessions()
	sessions.put("s-carry", agentloop.Session{ID: "s-carry"})
	steps := newMemorySteps()

	loop := agentloop.New(agentloop.Config{
		LLM:            fake,
		Sessions:       sessions,
		Steps:          steps,
		SandboxBuilder: dataBuilder{},
	})

	result, err := loop.Run(context.Background(), agentloop.RunRequest{
		SessionID: "s-carry",
		Message:   "build the blob",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// The carried snapshot is the ~2.5KB blob; the digest the model saw
	// is bounded (shapeOf caps depth/values). The delta must be large
	// and reported identically on the result and the finalize summary.
	snap := sessions.lastSnapshot("s-carry")
	if len(snap) < 1000 {
		t.Fatalf("expected a large carried snapshot, got %d bytes", len(snap))
	}
	if result.DataBytesCarried <= 0 {
		t.Fatalf("DataBytesCarried should be positive, got %d", result.DataBytesCarried)
	}
	if result.DataBytesCarried > int64(len(snap)) {
		t.Errorf("carried bytes %d cannot exceed the snapshot size %d (single digest injection)", result.DataBytesCarried, len(snap))
	}
	if result.DataBytesCarried < int64(len(snap))/2 {
		t.Errorf("carried bytes %d suspiciously small for a %d-byte snapshot — digest not subtracted correctly?", result.DataBytesCarried, len(snap))
	}
	fin := sessions.finalised("s-carry")
	if fin == nil || fin.DataBytesCarried != result.DataBytesCarried {
		t.Fatalf("finalize summary should carry the same measurement: %+v vs result %d", fin, result.DataBytesCarried)
	}

	// A run with no threaded data reports zero — no phantom savings.
	sessions.put("s-empty", agentloop.Session{ID: "s-empty"})
	fake2 := newFakeLLM("fake",
		llm.CompletionResponse{Content: "DONE\nHi."},
	)
	loop2 := agentloop.New(agentloop.Config{LLM: fake2, Sessions: sessions, Steps: steps, SandboxBuilder: dataBuilder{}})
	res2, err := loop2.Run(context.Background(), agentloop.RunRequest{SessionID: "s-empty", Message: "hi"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res2.DataBytesCarried != 0 {
		t.Errorf("no threaded data → zero carried bytes, got %d", res2.DataBytesCarried)
	}
}

// ---- test doubles ----

// fakeLLM is a scripted llm.Client: Complete/Stream return the queued
// responses in order and error once exhausted. Stream calls onChunk
// once with the full content, matching "best-effort incremental, full
// response always."
type fakeLLM struct {
	mu    sync.Mutex
	name  string
	resps []llm.CompletionResponse
	idx   int
	calls []llm.CompletionRequest
}

func newFakeLLM(name string, resps ...llm.CompletionResponse) *fakeLLM {
	return &fakeLLM{name: name, resps: resps}
}

func (f *fakeLLM) Name() string { return f.name }

func (f *fakeLLM) Complete(_ context.Context, req llm.CompletionRequest) (llm.CompletionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if f.idx >= len(f.resps) {
		return llm.CompletionResponse{}, fmt.Errorf("fakeLLM %s: no more responses queued (call %d)", f.name, f.idx+1)
	}
	resp := f.resps[f.idx]
	f.idx++
	return resp, nil
}

func (f *fakeLLM) Stream(ctx context.Context, req llm.CompletionRequest, onChunk func(string)) (llm.CompletionResponse, error) {
	resp, err := f.Complete(ctx, req)
	if err == nil && onChunk != nil && resp.Content != "" {
		onChunk(resp.Content)
	}
	return resp, err
}

func (f *fakeLLM) Calls() []llm.CompletionRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]llm.CompletionRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

type memorySessions struct {
	mu        sync.Mutex
	sessions  map[string]agentloop.Session
	final     map[string]*agentloop.FinalizeSummary
	snapshots map[string]json.RawMessage

	// Error injection for failure-path tests. Nil = succeed (default).
	getErr        error
	finalizeErr   error
	updateDataErr error
}

func newMemorySessions() *memorySessions {
	return &memorySessions{
		sessions:  make(map[string]agentloop.Session),
		final:     make(map[string]*agentloop.FinalizeSummary),
		snapshots: make(map[string]json.RawMessage),
	}
}

func (m *memorySessions) put(id string, s agentloop.Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[id] = s
}

func (m *memorySessions) Get(_ context.Context, id string) (agentloop.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.getErr != nil {
		return agentloop.Session{}, m.getErr
	}
	return m.sessions[id], nil
}

func (m *memorySessions) Exists(_ context.Context, id string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.sessions[id]
	return ok, nil
}

func (m *memorySessions) UpdateData(_ context.Context, id string, snap json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateDataErr != nil {
		return m.updateDataErr
	}
	m.snapshots[id] = snap
	return nil
}

func (m *memorySessions) Finalize(_ context.Context, id string, summary agentloop.FinalizeSummary) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.finalizeErr != nil {
		return m.finalizeErr
	}
	cp := summary
	m.final[id] = &cp
	return nil
}

func (m *memorySessions) finalised(id string) *agentloop.FinalizeSummary {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.final[id]
}

func (m *memorySessions) lastSnapshot(id string) json.RawMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshots[id]
}

type memorySteps struct {
	mu    sync.Mutex
	rows  map[string][]agentloop.RunStep
	order map[string][]string
	lastN int // most recent n passed to LastN, for window assertions

	// Error injection for failure-path tests. Nil = succeed (default).
	appendErr error
	lastNErr  error
}

func newMemorySteps() *memorySteps {
	return &memorySteps{
		rows:  make(map[string][]agentloop.RunStep),
		order: make(map[string][]string),
	}
}

func (m *memorySteps) Append(_ context.Context, step agentloop.RunStep) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.appendErr != nil {
		return m.appendErr
	}
	m.rows[step.SessionID] = append(m.rows[step.SessionID], step)
	m.order[step.SessionID] = append(m.order[step.SessionID], step.StepType)
	return nil
}

func (m *memorySteps) LastN(_ context.Context, id string, n int) ([]agentloop.RunStep, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastN = n
	if m.lastNErr != nil {
		return nil, m.lastNErr
	}
	all := m.rows[id]
	if len(all) <= n {
		out := make([]agentloop.RunStep, len(all))
		copy(out, all)
		return out, nil
	}
	out := make([]agentloop.RunStep, n)
	copy(out, all[len(all)-n:])
	return out, nil
}

func (m *memorySteps) lastNRequested() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastN
}

func (m *memorySteps) types(id string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.order[id]))
	copy(out, m.order[id])
	return out
}

// emptyBuilder returns a bare sandbox with no extra packs.
type emptyBuilder struct{}

func (emptyBuilder) Build(_ context.Context, _ agentloop.Session, _ agentloop.Scope, onEvent sandbox.OnEvent) (*sandbox.Sandbox, func(), error) {
	sb := sandbox.New()
	if onEvent != nil {
		sb.SetOnEvent(onEvent)
	}
	return sb, func() {}, nil
}

// dataBuilder returns a bare sandbox; the loop runs in run(args)→return mode,
// so state is carried by ExecuteTurn (a `function run(args)` returning a value),
// not a `data` global.
type dataBuilder struct{}

func (dataBuilder) Build(_ context.Context, _ agentloop.Session, _ agentloop.Scope, onEvent sandbox.OnEvent) (*sandbox.Sandbox, func(), error) {
	sb := sandbox.New()
	if onEvent != nil {
		sb.SetOnEvent(onEvent)
	}
	return sb, func() {}, nil
}

// --- tiny helpers to keep assertions readable ---

func eventTypes(events []agentloop.RunEvent) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.Type)
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestRun_ProseAnswer: once a script has run, a reply without one is the
// answer; before any script it is still corrected, and a tool call never
// counts as an answer.
func TestRun_ProseAnswer(t *testing.T) {
	newLoop := func(replies ...string) (agentloop.Loop, *fakeLLM) {
		var resps []llm.CompletionResponse
		for _, r := range replies {
			resps = append(resps, llm.CompletionResponse{Content: r})
		}
		fake := newFakeLLM("fake", resps...)
		sessions := newMemorySessions()
		sessions.put("s1", agentloop.Session{ID: "s1"})
		return agentloop.New(agentloop.Config{
			LLM: fake, Sessions: sessions, Steps: newMemorySteps(),
			SandboxBuilder: emptyBuilder{}, RequireScript: true, ProseAnswer: true,
		}), fake
	}
	script := "```javascript\nconsole.log('looked')\n```"

	loop, _ := newLoop(script, "The answer is **620** — `MaxNodes = 32`.")
	res, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi"})
	if err != nil || res.Status != "completed" || !strings.Contains(res.FinalText, "`MaxNodes = 32`") {
		t.Errorf("prose after a script: %v %q %q", err, res.Status, res.FinalText)
	}

	loop, fake := newLoop("I would look at the files.", script, "Done.")
	res, err = loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi"})
	if err != nil || res.FinalText != "Done." || len(fake.Calls()) != 3 {
		t.Errorf("prose before a script should be corrected first: %v %q calls=%d", err, res.FinalText, len(fake.Calls()))
	}

	loop, fake = newLoop(script, "<tool_call>\n<function=readFile>\n</function>\n</tool_call>", "Done.")
	res, _ = loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi"})
	if res.FinalText != "Done." || len(fake.Calls()) != 3 {
		t.Errorf("a tool call was taken as the answer: %q calls=%d", res.FinalText, len(fake.Calls()))
	}
}

// TestRun_ErrorsNameTheScriptLineAndRepeats: a failing script's result
// points at the model's own line, and a second identical failure says so.
// The script before the cap also carries the budget notice.
func TestRun_ErrorsNameTheScriptLineAndRepeats(t *testing.T) {
	broken := "```javascript\nconst a = 1;\nconst b = `x `y` z`;\n```"
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: broken},
		llm.CompletionResponse{Content: broken},
		llm.CompletionResponse{Content: "DONE\nok"},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	loop := agentloop.New(agentloop.Config{
		LLM: fake, Sessions: sessions, Steps: newMemorySteps(),
		SandboxBuilder: emptyBuilder{}, MaxIterations: 4, BudgetNotice: true, ProseAnswer: true,
	})
	var results []string
	_, _ = loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi", OnEvent: func(e agentloop.RunEvent) {
		if e.Type == "execute_js_result" {
			results = append(results, e.Content)
		}
	}})
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if !strings.Contains(results[0], "at line 2 of your script") || !strings.Contains(results[0], "blocks[0]") {
		t.Errorf("first error lacks the script line or hint:\n%s", results[0])
	}
	if strings.Contains(results[0], "same error") || !strings.Contains(results[1], "same error as your previous script") {
		t.Errorf("repeat detection wrong:\n%s\n---\n%s", results[0], results[1])
	}
	if strings.Contains(results[0], "NOTICE") || !strings.Contains(results[1], "two replies are left") || !strings.Contains(results[1], "plain markdown") || strings.Contains(results[1], "answer(result)") {
		t.Errorf("budget notice wrong:\n%s\n---\n%s", results[0], results[1])
	}
}

// TestRun_FileBlocksAreWrittenBeforeTheScript: a labelled fence goes
// through the sandbox's writeFile ahead of the script, a reply of only
// file blocks is an action rather than prose, and an error in the model's
// script still names its own line.
func TestRun_FileBlocksAreWrittenBeforeTheScript(t *testing.T) {
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: "```go a.go\npackage a // `raw`\n```"},
		llm.CompletionResponse{Content: "```go b.go\npackage b\n```\n```javascript\nconst x = 1;\nnope();\n```"},
		llm.CompletionResponse{Content: "Done."},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	written := map[string]string{}
	builder := writeFileBuilder{written: written}
	loop := agentloop.New(agentloop.Config{
		LLM: fake, Sessions: sessions, Steps: newMemorySteps(), SandboxBuilder: builder,
		RequireScript: true, ProseAnswer: true, FileBlocks: true,
	})
	var results []string
	res, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi", OnEvent: func(e agentloop.RunEvent) {
		if e.Type == "execute_js_result" {
			results = append(results, e.Content)
		}
	}})
	if err != nil || res.FinalText != "Done." {
		t.Fatalf("run: %v %q", err, res.FinalText)
	}
	if written["a.go"] != "package a // `raw`" || written["b.go"] != "package b" {
		t.Errorf("written = %q", written)
	}
	if len(results) != 2 || !strings.Contains(results[0], "wrote a.go") {
		t.Fatalf("results = %q", results)
	}
	if !strings.Contains(results[1], "at line 2 of your script") {
		t.Errorf("error does not name the model's line:\n%s", results[1])
	}
}

// writeFileBuilder gives the sandbox a writeFile that records its calls.
type writeFileBuilder struct{ written map[string]string }

func (b writeFileBuilder) Build(_ context.Context, _ agentloop.Session, _ agentloop.Scope, onEvent sandbox.OnEvent) (*sandbox.Sandbox, func(), error) {
	sb := sandbox.New(sandbox.Pack{Name: "fs", Register: func(rt *goja.Runtime, _ *sandbox.Sandbox) {
		_ = rt.Set("writeFile", func(path, content string) bool { b.written[path] = content; return true })
	}})
	sb.SetOnEvent(onEvent)
	return sb, func() {}, nil
}

// TestRun_ProseAnswerRefusesACodeOnlyReply: a reply that is mostly an
// unlabelled code block is not taken as the answer; the model is told how
// to label a file or finish in words.
func TestRun_ProseAnswerRefusesACodeOnlyReply(t *testing.T) {
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: "```javascript\nconsole.log('looked')\n```"},
		llm.CompletionResponse{Content: "```go\nratelimit.go\n```\n```go\npackage ratelimit\n\nfunc Allow() error { return nil }\n```"},
		llm.CompletionResponse{Content: "I added AllowN to memory.go and its tests pass."},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	loop := agentloop.New(agentloop.Config{
		LLM: fake, Sessions: sessions, Steps: newMemorySteps(), SandboxBuilder: emptyBuilder{},
		RequireScript: true, ProseAnswer: true, FileBlocks: true,
	})
	res, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi"})
	if err != nil || res.FinalText != "I added AllowN to memory.go and its tests pass." {
		t.Fatalf("run: %v %q", err, res.FinalText)
	}
	msgs := fake.Calls()[2].Messages
	if last := msgs[len(msgs)-1].Content; !strings.Contains(last, "path on the fence line itself") {
		t.Errorf("third call should carry the code-only correction, got %q", last)
	}
}

// TestRun_UserOutput: print writes for the user and not the model's log,
// return still threads, done() ends with everything printed plus its
// message, and an empty done() is refused with a notice.
func TestRun_UserOutput(t *testing.T) {
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: "```javascript\ndone();\n```"},
		llm.CompletionResponse{Content: "```javascript\nprint('Renamed Title to Name.');\nconsole.log('seen by the model');\nreturn { n: 2 };\n```"},
		llm.CompletionResponse{Content: "```javascript\nprint(`Files: ${args.n}`);\nexit('Tests pass.');\n```"},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	loop := agentloop.New(agentloop.Config{
		LLM: fake, Sessions: sessions, Steps: newMemorySteps(), SandboxBuilder: emptyBuilder{},
		RequireScript: true, UserOutput: true,
	})
	var results []string
	res, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "hi", OnEvent: func(e agentloop.RunEvent) {
		if e.Type == "execute_js_result" {
			results = append(results, e.Content)
		}
	}})
	if err != nil || res.Status != "completed" {
		t.Fatalf("run: %v %q", err, res.Status)
	}
	if res.FinalText != "Renamed Title to Name.\nFiles: 2\nTests pass." {
		t.Errorf("final = %q", res.FinalText)
	}
	if len(results) != 3 || !strings.Contains(results[0], "exit() was called with nothing printed") {
		t.Fatalf("an empty done() was not refused: %q", results)
	}
	if strings.Contains(results[1], "Renamed") || !strings.Contains(results[1], "seen by the model") {
		t.Errorf("print leaked into the model's log, or log is missing: %q", results[1])
	}
}

// TestRun_ClarifyAndOpenAPI: the first plain-text reply is recorded as the
// model's understanding and answered with "proceed", and a guessed
// function is named without calling the declarations complete.
func TestRun_ClarifyAndOpenAPI(t *testing.T) {
	fake := newFakeLLM("fake",
		llm.CompletionResponse{Content: "1) Rename Title. 2) go build passes. 3) Unclear whether tests exist."},
		llm.CompletionResponse{Content: "```javascript\nconst r = readJSON('a.json');\n```"},
		llm.CompletionResponse{Content: "```javascript\nprint('done'); done();\n```"},
	)
	sessions := newMemorySessions()
	sessions.put("s1", agentloop.Session{ID: "s1"})
	loop := agentloop.New(agentloop.Config{
		LLM: fake, Sessions: sessions, Steps: newMemorySteps(), SandboxBuilder: emptyBuilder{},
		RequireScript: true, UserOutput: true, Clarify: true, OpenAPI: true,
	})
	var understanding string
	var results []string
	res, err := loop.Run(context.Background(), agentloop.RunRequest{SessionID: "s1", Message: "rename it", OnEvent: func(e agentloop.RunEvent) {
		switch e.Type {
		case "understanding":
			understanding = e.Content
		case "execute_js_result":
			results = append(results, e.Content)
		}
	}})
	if err != nil || res.FinalText != "done" {
		t.Fatalf("run: %v %q", err, res.FinalText)
	}
	if !strings.Contains(understanding, "Unclear whether tests exist") {
		t.Errorf("understanding = %q", understanding)
	}
	first := fake.Calls()[0].Messages
	if last := first[len(first)-1].Content; !strings.Contains(last, "anything unclear") {
		t.Errorf("first call does not ask for the understanding: %q", last)
	}
	if len(results) == 0 || !strings.Contains(results[0], "`readJSON` is not available") || strings.Contains(results[0], "complete list") {
		t.Errorf("open API hint wrong: %q", results)
	}
}
