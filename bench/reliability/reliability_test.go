package reliability

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeModel is an OpenAI-wire endpoint that answers from a script: reply(n,
// req) gives the n-th reply of a run (0-based), as content or tool calls.
// Every arm is driven through it end to end, so a broken driver or check
// shows here, before a paid call.
type fakeModel struct {
	mu    sync.Mutex
	n     int
	reply func(n int, req map[string]any) map[string]any
	reqs  []map[string]any
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	n := f.n
	f.n++
	f.reqs = append(f.reqs, req)
	f.mu.Unlock()
	msg := f.reply(n, req)
	resp := map[string]any{
		"choices": []any{map[string]any{"message": msg, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 100, "completion_tokens": 10},
	}
	if req["stream"] == true {
		// agentloop streams: one content delta, the usage, then [DONE].
		w.Header().Set("Content-Type", "text/event-stream")
		c, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": msg["content"]}}}})
		u, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}, "usage": resp["usage"]})
		_, _ = w.Write([]byte("data: " + string(c) + "\n\ndata: " + string(u) + "\n\ndata: [DONE]\n\n"))
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func script(code string) map[string]any {
	return map[string]any{"role": "assistant", "content": "```javascript\n" + code + "\n```"}
}

func text(s string) map[string]any { return map[string]any{"role": "assistant", "content": s} }

func task(t *testing.T, id string) Task {
	for _, tk := range Tasks() {
		if tk.ID == id {
			return tk
		}
	}
	t.Fatalf("no task %s", id)
	return Task{}
}

var testModel = Model{Name: "fake", ID: "fake", InPerM: 1, OutPerM: 1}

func serve(t *testing.T, f *fakeModel) *Chat {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Chat{BaseURL: srv.URL, APIKey: "k"}
}

// The carry task, solved the way each arm is meant to be used: read once,
// keep it, read the question, answer from what was kept.
func TestCodeArmsCarry(t *testing.T) {
	answer := "const m = rows.filter(o => o.status === 'paid' && o.date.startsWith('2025-03'));" +
		"answer(m.length + ' orders, ' + (m.reduce((s, o) => s + o.amountCents, 0) / 100).toFixed(2) + ' EUR');"
	cases := map[string][]string{
		ArmState: {
			"state.rows = loadOrderHistory(); log(getQuestion());",
			"const rows = state.rows; " + answer,
		},
		ArmMain: {
			"function main(state) { state.rows = loadOrderHistory(); log(getQuestion()); return state; }",
			"function main(state) { const rows = state.rows; " + answer + " return state; }",
		},
		ArmData: {
			"data.rows = loadOrderHistory(); log(getQuestion());",
			"const rows = data.rows; " + answer,
		},
	}
	for arm, steps := range cases {
		t.Run(arm, func(t *testing.T) {
			f := &fakeModel{reply: func(n int, _ map[string]any) map[string]any { return script(steps[min(n, len(steps)-1)]) }}
			r := RunCode(context.Background(), serve(t, f), testModel, arm, task(t, "04-carry"))
			if !r.Pass || r.Turns != 2 || !r.FirstValid {
				t.Fatalf("%s: pass=%v turns=%d failure=%s answers=%q", arm, r.Pass, r.Turns, r.Failure, r.Answers)
			}
			// The second call sees what carried: its shape for B and C, its
			// values for D.
			second := lastUser(f.reqs[1])
			switch arm {
			case ArmData:
				if !strings.HasPrefix(second, "data (current value):") || !strings.Contains(second, "O-1001") {
					t.Errorf("D shows %q", truncate(second, 120))
				}
			default:
				if !strings.Contains(second, "(shape only)") || strings.Contains(second, "O-1001") {
					t.Errorf("%s shows %q", arm, truncate(second, 120))
				}
			}
		})
	}
}

// Under A nothing carries, so the second script finds no rows and the
// history cannot be read again: the arm's predicted failure.
func TestScriptArmCarriesNothing(t *testing.T) {
	steps := []string{"state.rows = loadOrderHistory(); log(getQuestion());", "log(typeof state.rows); state.rows = loadOrderHistory();", "answer('none')"}
	f := &fakeModel{reply: func(n int, _ map[string]any) map[string]any { return script(steps[min(n, 2)]) }}
	r := RunCode(context.Background(), serve(t, f), testModel, ArmScript, task(t, "04-carry"))
	if r.Pass || r.Failure != "lost_data" {
		t.Fatalf("pass=%v failure=%s", r.Pass, r.Failure)
	}
	if !strings.Contains(r.Transcript[5].Content.(string), "undefined") {
		t.Errorf("state carried under A: %v", r.Transcript[5].Content)
	}
}

// A main that forgets to return loses everything it held.
func TestMainWithoutReturnLosesState(t *testing.T) {
	steps := []string{"function main(state) { state.rows = loadOrderHistory(); }", "function main(state) { answer(String(state.rows)); return state; }"}
	f := &fakeModel{reply: func(n int, _ map[string]any) map[string]any { return script(steps[min(n, 1)]) }}
	r := RunCode(context.Background(), serve(t, f), testModel, ArmMain, task(t, "04-carry"))
	if r.Answers[0] != "undefined" {
		t.Fatalf("answer %q, want the lost rows to read undefined", r.Answers[0])
	}
}

func TestRecoverAndFollowup(t *testing.T) {
	steps := []string{
		"log(getStock('LMP-200').available)",
		"log(getStock('LMP-200').available)",
		"answer('37')",
	}
	f := &fakeModel{reply: func(n int, _ map[string]any) map[string]any { return script(steps[min(n, 2)]) }}
	r := RunCode(context.Background(), serve(t, f), testModel, ArmState, task(t, "05-recover"))
	if !r.Pass || r.ScriptErrors != 1 || r.FirstValid {
		t.Fatalf("pass=%v errors=%d firstValid=%v", r.Pass, r.ScriptErrors, r.FirstValid)
	}

	fu := []map[string]any{
		script("const c = findCustomer('Hoffmann GmbH'); state.id = c.id; answer((listOrders({customerId: c.id, status: 'paid', month: '2025-03'}).reduce((s,o)=>s+o.amountCents,0)/100).toFixed(2))"),
		script("answer((listOrders({customerId: state.id, status: 'paid', month: '2025-05'}).reduce((s,o)=>s+o.amountCents,0)/100).toFixed(2))"),
	}
	f2 := &fakeModel{reply: func(n int, _ map[string]any) map[string]any { return fu[min(n, 1)] }}
	r = RunCode(context.Background(), serve(t, f2), testModel, ArmState, task(t, "07-followup"))
	if !r.Pass || len(r.Answers) != 2 {
		t.Fatalf("follow-up: pass=%v answers=%q", r.Pass, r.Answers)
	}
}

func TestNoFenceIsAnAnswer(t *testing.T) {
	f := &fakeModel{reply: func(int, map[string]any) map[string]any { return text("391") }}
	r := RunCode(context.Background(), serve(t, f), testModel, ArmScript, task(t, "08-notool"))
	if !r.Pass || r.Turns != 1 {
		t.Fatalf("pass=%v turns=%d", r.Pass, r.Turns)
	}
}

func TestToolsArm(t *testing.T) {
	f := &fakeModel{reply: func(n int, req map[string]any) map[string]any {
		switch n {
		case 0:
			return map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "1", "type": "function", "function": map[string]any{"name": "findCustomer", "arguments": `{"name":"Hoffmann GmbH"}`}},
			}}
		case 1:
			return map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "2", "type": "function", "function": map[string]any{"name": "createTicket", "arguments": `{"customerId":"C-007","priority":"high","subject":"Invoice 2025-03 missing","tags":["urgent","billing"]}`}},
			}}
		}
		return text("Ticket T-5001 is open.")
	}}
	r := RunTools(context.Background(), serve(t, f), testModel, task(t, "06-args"))
	if !r.Pass || r.Turns != 3 || !r.FirstValid {
		t.Fatalf("pass=%v turns=%d failure=%s answers=%q calls=%v", r.Pass, r.Turns, r.Failure, r.Answers, r.Calls)
	}
	tools := f.reqs[0]["tools"].([]any)
	if len(tools) != len(core) {
		t.Errorf("%d tools offered, want %d", len(tools), len(core))
	}
}

func TestToolsArmUnknownTool(t *testing.T) {
	f := &fakeModel{reply: func(n int, _ map[string]any) map[string]any {
		if n == 0 {
			return map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "1", "type": "function", "function": map[string]any{"name": "getCity", "arguments": `{}`}},
			}}
		}
		return text("Hamburg")
	}}
	r := RunTools(context.Background(), serve(t, f), testModel, task(t, "01-lookup"))
	if r.Pass || r.Failure != "unknown_function" {
		t.Fatalf("pass=%v failure=%s", r.Pass, r.Failure)
	}
}

func TestLoopArm(t *testing.T) {
	steps := []map[string]any{
		script("const c = findCustomer('Weber KG'); state.t = createTicket({customerId: c.id, priority: 'normal', subject: 'Address change'});"),
		script("answer('Created ' + state.t.ticketId)"),
	}
	f := &fakeModel{reply: func(n int, _ map[string]any) map[string]any { return steps[min(n, 1)] }}
	srv := httptest.NewServer(f)
	defer srv.Close()
	r := RunLoop(context.Background(), srv.URL, "k", testModel, task(t, "10-once"))
	if !r.Pass || r.Turns != 2 || r.PromptTokens != 200 {
		t.Fatalf("pass=%v turns=%d prompt=%d status=%s err=%s answers=%q", r.Pass, r.Turns, r.PromptTokens, r.Status, r.Error, r.Answers)
	}
}

func TestSelectTaskOffersTwenty(t *testing.T) {
	if n := len(task(t, "09-select").Functions); n != 20 {
		t.Fatalf("%d functions, want 20", n)
	}
}

func TestHasNumber(t *testing.T) {
	for _, c := range []struct {
		s    string
		want float64
		ok   bool
	}{
		{"8247.10 EUR", 8247.10, true},
		{"8.247,10 €", 8247.10, true},
		{"€8,247.10", 8247.10, true},
		{"8247,1", 8247.10, true},
		{"8247", 8247.10, false},
		{"23 orders totalling 20,691.30 euros", 23, true},
		{"there are 6", 6, true},
		{"16", 6, false},
		{"20\u202f691,30 €", 20691.30, true},
		{"20 691.30", 20691.30, true},
	} {
		if got := hasNumber(c.s, c.want, 0.005); got != c.ok {
			t.Errorf("hasNumber(%q, %v) = %v", c.s, c.want, got)
		}
	}
}

func TestNormalizedAnswers(t *testing.T) {
	if !contains("Estimated arrival: 2025\u201106\u201114", "2025-06-14") {
		t.Error("a non-breaking hyphen hides the date")
	}
}

func TestDeclarations(t *testing.T) {
	d := Declarations([]Function{fnFindCustomer, fnCreateTicket, fnLoadHistory})
	for _, want := range []string{
		"declare function findCustomer(name: string): {",
		`declare function createTicket(args: { customerId: string; priority: "low" | "normal" | "high"; subject: string; tags?: string[] }): { ticketId: string };`,
		"declare function loadOrderHistory(): {",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("missing %q in\n%s", want, d)
		}
	}
}

func TestReport(t *testing.T) {
	rs := []Result{
		{Arm: ArmState, Model: "m", Task: "01-lookup", Rep: 1, Pass: true, Status: "answered"},
		{Arm: ArmTools, Model: "m", Task: "01-lookup", Rep: 1, Pass: false, Failure: "wrong_answer", Status: "answered"},
		{Arm: ArmTools, Model: "m", Task: "01-lookup", Rep: 2, Status: "llm_error"},
	}
	out := Report(rs, []Model{{Name: "m"}})
	for _, want := range []string{"1 of them infrastructure errors", "| E-tools | 1 | 1 | 0 |", "| wrong_answer |"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func lastUser(req map[string]any) string {
	msgs := req["messages"].([]any)
	m := msgs[len(msgs)-1].(map[string]any)
	s, _ := m["content"].(string)
	return s
}
