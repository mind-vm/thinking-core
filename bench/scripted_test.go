package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// scripted is an OpenAI-wire endpoint that plays a model for both agents
// on two tasks, with no API key and no network. A request carrying
// `tools` is the baseline and gets tool_calls; one without is
// thinking-core and gets a ```javascript block over SSE. The replies are
// computed from the conversation so far — the tool results and the
// script output the agent sent back — so a test passing proves the data
// actually flowed through each agent's loop, not that a canned answer
// was echoed.
//
// Model "scripted-wrong" plays a model that does the same work and then
// misreports the result, to prove the checker marks it wrong.
//
// Usage is reported as request bytes / 4, so the token columns respond
// to what each agent actually put in its prompt.
type scripted struct {
	t     *testing.T
	tasks []Task
}

func (s *scripted) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model    string            `json:"model"`
		Stream   bool              `json:"stream"`
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			Role      string         `json:"role"`
			Content   *string        `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"messages"`
	}
	var raw strings.Builder
	if err := json.NewDecoder(teeReader{r.Body, &raw}).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	text := func(i int) string {
		if c := req.Messages[i].Content; c != nil {
			return *c
		}
		return ""
	}
	var user string
	assistantTurns := 0
	var toolResults []string
	var userTurns string // every user turn, which carries script output back
	for i, m := range req.Messages {
		switch m.Role {
		case "user":
			if user == "" {
				user = text(i)
			}
			userTurns += text(i) + "\n"
		case "assistant":
			assistantTurns++
		case "tool":
			toolResults = append(toolResults, text(i))
		}
	}
	wrong := req.Model == "scripted-wrong"
	usage := map[string]int{"prompt_tokens": raw.Len() / 4}

	if len(req.Tools) > 0 {
		calls, final := s.baseline(user, assistantTurns, toolResults, wrong)
		msg := map[string]any{"role": "assistant", "content": nil}
		if calls != nil {
			msg["tool_calls"] = calls
		} else {
			msg["content"] = final
		}
		b, _ := json.Marshal(msg)
		usage["completion_tokens"] = len(b) / 4
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": msg, "finish_reason": "stop"}},
			"usage":   usage,
		})
		return
	}

	if !req.Stream {
		s.t.Errorf("thinking-core sent a non-streaming request")
	}
	reply := s.codeAction(user, assistantTurns, userTurns, wrong)
	usage["completion_tokens"] = len(reply) / 4
	w.Header().Set("Content-Type", "text/event-stream")
	frame := func(v any) {
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	}
	// Two content frames, so the client's delta accumulation is exercised.
	half := len(reply) / 2
	frame(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": reply[:half]}}}})
	frame(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": reply[half:]}, "finish_reason": "stop"}}})
	frame(map[string]any{"choices": []any{}, "usage": usage})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func (s *scripted) taskOf(user string) string {
	for _, t := range s.tasks {
		if user == t.Prompt {
			return t.ID
		}
	}
	s.t.Errorf("scripted endpoint: unknown task prompt %q", user)
	return ""
}

func toolCall(id, name string, args map[string]any) map[string]any {
	b, _ := json.Marshal(args)
	return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": string(b)}}
}

// baseline plays the tool-calling model: plan the calls from what came
// back so far, then answer from the tool results.
func (s *scripted) baseline(user string, turn int, results []string, wrong bool) ([]any, string) {
	parse := func(r string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(r), &m); err != nil {
			s.t.Errorf("tool result is not JSON: %v: %s", err, r)
		}
		if e, ok := m["error"]; ok {
			s.t.Errorf("tool returned an error: %v", e)
		}
		return m
	}
	switch s.taskOf(user) {
	case "lookup":
		if turn == 0 {
			return []any{toolCall("c1", "get_customer", map[string]any{"id": "C0042"})}, ""
		}
		c := parse(results[0])
		if wrong {
			c["country"] = "XX"
		}
		return nil, fmt.Sprintf(`{"plan": %q, "country": %q}`, c["plan"], c["country"])
	case "overdue_de":
		switch turn {
		case 0:
			// Parallel calls in one turn, as a capable model emits them.
			return []any{
				toolCall("c1", "list_customers", map[string]any{"country": "DE"}),
				toolCall("c2", "list_invoices", map[string]any{"status": "overdue"}),
			}, ""
		case 1:
			var calls []any
			cust, inv := parse(results[0]), parse(results[1])
			for p := 2; p <= int(cust["total_pages"].(float64)); p++ {
				calls = append(calls, toolCall(fmt.Sprintf("c%d", 10+p), "list_customers", map[string]any{"country": "DE", "page": p}))
			}
			for p := 2; p <= int(inv["total_pages"].(float64)); p++ {
				calls = append(calls, toolCall(fmt.Sprintf("i%d", 10+p), "list_invoices", map[string]any{"status": "overdue", "page": p}))
			}
			if len(calls) == 0 {
				s.t.Errorf("overdue_de needed no second page; the scripted baseline no longer exercises pagination")
			}
			return calls, ""
		default:
			de := map[string]bool{}
			var invoices []any
			for _, r := range results {
				m := parse(r)
				for _, c := range asSlice(m["customers"]) {
					de[c.(map[string]any)["id"].(string)] = true
				}
				invoices = append(invoices, asSlice(m["invoices"])...)
			}
			n, sum := 0, 0
			for _, in := range invoices {
				m := in.(map[string]any)
				if de[m["customer_id"].(string)] {
					n++
					sum += int(m["amount_cents"].(float64))
				}
			}
			if wrong {
				sum++
			}
			return nil, fmt.Sprintf("Here is the result:\n```json\n{\"invoice_count\": %d, \"total_cents\": %d}\n```", n, sum)
		}
	}
	return nil, "{}"
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

// codeAction plays the thinking-core model. overdue_de takes two
// scripts so the carry through `state` between turns is exercised: the
// first fetches and logs, the second computes from state alone.
func (s *scripted) codeAction(user string, turn int, userTurns string, wrong bool) string {
	bad := ""
	if wrong {
		bad = "+ 1"
	}
	js := func(code string) string { return "```javascript\n" + code + "\n```" }
	switch s.taskOf(user) {
	case "lookup":
		if wrong {
			return js(`const db = require('mcp/data');
const c = db.getCustomer({ id: 'C0042' });
answer({ plan: c.plan, country: 'XX' });`)
		}
		return js(`const db = require('mcp/data');
const c = db.getCustomer({ id: 'C0042' });
answer({ plan: c.plan, country: c.country });`)
	case "overdue_de":
		if turn == 0 {
			return js(`const db = require('mcp/data');
function all(fn, args, key) {
  let rows = [], page = 1, r;
  do { r = fn(Object.assign({}, args, { page })); rows = rows.concat(r[key]); page++; } while (page <= r.total_pages);
  return rows;
}
state.de = all(db.listCustomers, { country: 'DE' }, 'customers').map(c => c.id);
state.overdue = all(db.listInvoices, { status: 'overdue' }, 'invoices');
log('DE customers', state.de.length, 'overdue invoices', state.overdue.length);`)
		}
		if !strings.Contains(userTurns, "DE customers") || !strings.Contains(userTurns, "state (shape only)") {
			s.t.Errorf("second turn did not see the first script's log output and state shape: %q", userTurns)
		}
		return js(`const de = new Set(state.de);
const mine = state.overdue.filter(i => de.has(i.customer_id));
answer({ invoice_count: mine.length, total_cents: mine.reduce((s, i) => s + i.amount_cents, 0) ` + bad + ` });`)
	}
	return "{}"
}

type teeReader struct {
	r interface{ Read([]byte) (int, error) }
	w *strings.Builder
}

func (t teeReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.w.Write(p[:n])
	return n, err
}

func TestBothAgentsAgainstScriptedEndpoint(t *testing.T) {
	d := Generate(1)
	var tasks []Task
	for _, id := range []string{"lookup", "overdue_de"} {
		task, _ := TaskByID(Tasks(d), id)
		tasks = append(tasks, task)
	}
	srv := httptest.NewServer(&scripted{t: t, tasks: tasks})
	defer srv.Close()
	ep := Endpoint{BaseURL: srv.URL, APIKey: "test", HTTPClient: srv.Client(), MaxTurns: 6}

	var streamed []Result
	results := Plan{
		Data: d, Tasks: tasks, Agents: []Agent{CodeAction{Endpoint: ep}, ToolCalling{Endpoint: ep}},
		Models: []string{"scripted", "scripted-wrong"}, Reps: 2, Parallel: 4,
		OnResult: func(r Result) { streamed = append(streamed, r) },
	}.Run(context.Background())

	if want := 2 * 2 * 2 * 2; len(results) != want || len(streamed) != want {
		t.Fatalf("%d results (%d streamed), want %d", len(results), len(streamed), want)
	}
	// Expected turns and tool calls per (task, agent), from the scripts
	// above: the baseline pays a round-trip per pagination step, the
	// script pages inside one turn.
	type key struct{ task, agent string }
	wantTurns := map[key]int{
		{"lookup", "thinking-core"}: 1, {"lookup", "tool-calling"}: 2,
		{"overdue_de", "thinking-core"}: 2, {"overdue_de", "tool-calling"}: 3,
	}
	tokens := map[key]int{}
	for _, r := range results {
		k := key{r.Task, r.Agent}
		if r.Error != "" {
			t.Errorf("%v %s: error %s", k, r.Model, r.Error)
		}
		if r.Correct != (r.Model == "scripted") {
			t.Errorf("%v %s rep %d: correct=%v (%s); answer %q", k, r.Model, r.Rep, r.Correct, r.CheckDetail, r.Answer)
		}
		if r.Turns != wantTurns[k] {
			t.Errorf("%v: %d turns, want %d", k, r.Turns, wantTurns[k])
		}
		if r.ToolCalls == 0 || r.PromptTokens == 0 || r.CompletionTokens == 0 {
			t.Errorf("%v: metrics not collected: %+v", k, r)
		}
		if r.Seed != 1 {
			t.Errorf("%v: seed %d", k, r.Seed)
		}
		tokens[k] = r.PromptTokens
	}
	// The mechanism the benchmark measures, visible even with a scripted
	// model: on the join, the baseline's prompt carries every row it
	// fetched, thinking-core's carries a shape digest and a log line.
	if tc, bl := tokens[key{"overdue_de", "thinking-core"}], tokens[key{"overdue_de", "tool-calling"}]; tc >= bl {
		t.Errorf("overdue_de prompt tokens: thinking-core %d >= tool-calling %d", tc, bl)
	}

	var b strings.Builder
	if err := WriteSummary(&b, "scripted", results); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## scripted\n", "## scripted-wrong\n", "| thinking-core | 4/4 | 100% |", "| tool-calling | 0/4 | 0% |"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("summary lacks %q:\n%s", want, b.String())
		}
	}
	var jl strings.Builder
	if err := WriteJSONL(&jl, results); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(jl.String(), "\n"); n != len(results) {
		t.Errorf("JSONL has %d lines for %d results", n, len(results))
	}
}

// TestBaselineSurfacesToolErrors checks the baseline hands a bad call's
// error back to the model as a tool result rather than failing the run —
// the recovery path an idiomatic tool-calling loop has.
func TestBaselineSurfacesToolErrors(t *testing.T) {
	byName := map[string]Tool{}
	for _, tl := range Tools() {
		byName[tl.Name] = tl
	}
	var call wireToolCall
	call.Function.Name = "list_invoices"
	call.Function.Arguments = `{"date":"2026-03"}`
	if got := runTool(byName, Generate(1), call); !strings.Contains(got, `"error"`) || !strings.Contains(got, "month") {
		t.Errorf("bad argument: %s", got)
	}
	call.Function.Name = "nope"
	if got := runTool(byName, Generate(1), call); !strings.Contains(got, "unknown tool") {
		t.Errorf("unknown tool: %s", got)
	}
}
