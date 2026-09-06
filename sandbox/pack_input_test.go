package sandbox

import (
	"errors"
	"strings"
	"testing"
)

// answering is a test InputRequester with a fixed reply that records
// what it was asked.
type answering struct {
	reply any
	err   error
	seen  []InputRequestData
}

func (a *answering) Request(req InputRequestData) (any, error) {
	a.seen = append(a.seen, req)
	if a.err != nil {
		return nil, a.err
	}
	return a.reply, nil
}

// askIn builds a sandbox with only the input pack and collects its
// events, so a test can assert on both what the script got and what
// the trace saw.
func askIn(requester InputRequester) (*Sandbox, *[]Event) {
	var events []Event
	s := New(InputPack(requester))
	s.SetOnEvent(func(e Event) { events = append(events, e) })
	return s, &events
}

// kinds lists the events the pack itself emitted. The sandbox brackets
// every run in script / script_result and reports a thrown turn as an
// error of its own; those are the executor's, not ask()'s.
func kinds(events []Event) []EventKind {
	out := make([]EventKind, 0, len(events))
	for _, e := range events {
		switch e.Kind {
		case EventScript, EventScriptResult:
			continue
		case EventError:
			if !strings.HasPrefix(e.Summary, "ask") {
				continue
			}
		}
		out = append(out, e.Kind)
	}
	return out
}

// The nil-requester contract: the model is told about ask() so scripts
// written for an interactive host still parse, and the call fails with
// a reason rather than a ReferenceError.
func TestAskIsDocumentedButRefusedWithoutARequester(t *testing.T) {
	s, _ := askIn(nil)

	if !strings.Contains(s.SystemPrompt(), "declare function ask(") {
		t.Error("ask() is not documented; a script written for an interactive host would hit a ReferenceError here")
	}
	_, err := s.Execute(`ask({type: "confirm", message: "ok?"})`)
	if err == nil || !strings.Contains(err.Error(), "no InputRequester") {
		t.Fatalf("want the no-requester error, got %v", err)
	}
}

func TestAskConfirmRoundTripsTheAnswer(t *testing.T) {
	r := &answering{reply: true}
	s, events := askIn(r)

	out, err := s.Execute(`ask({type: "confirm", message: "Continue?"})`)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if out != "true" {
		t.Fatalf("the script got %q, want the requester's true", out)
	}
	if len(r.seen) != 1 || r.seen[0].InputType != "confirm" || r.seen[0].Message != "Continue?" {
		t.Fatalf("the requester saw %+v", r.seen)
	}

	// The event pair is the seam this pack was promoted into: EventAsk
	// and EventAskResult existed on the platform before ask() did, and a
	// trace consumer is entitled to see both around every question.
	if got := kinds(*events); len(got) != 2 || got[0] != EventAsk || got[1] != EventAskResult {
		t.Fatalf("events %v, want [ask ask_result]", got)
	}
	for _, e := range *events {
		if e.Kind == EventAskResult && e.Result != "true" {
			t.Errorf("ask_result carries %q, want the answer", e.Result)
		}
	}
}

func TestAskChoiceAndTextCarryTheirFields(t *testing.T) {
	r := &answering{reply: "b"}
	s, _ := askIn(r)

	out, err := s.Execute(`ask({type: "choice", message: "Which?", options: ["a", "b"]})`)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if out != "b" {
		t.Errorf("the script got %q, want b", out)
	}
	if got := r.seen[0].Options; len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("options reached the requester as %v", got)
	}

	if _, err := s.Execute(`ask({type: "text", message: "Name?", placeholder: "e.g. Ada"})`); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if got := r.seen[1].Placeholder; got != "e.g. Ada" {
		t.Errorf("placeholder reached the requester as %q", got)
	}
}

// A human-loop round trip is the most expensive thing a script can do,
// so a malformed call fails before it costs one. Each error names the
// field, because the model has to fix it on the next turn.
func TestAskValidatesBeforeTroublingAHuman(t *testing.T) {
	cases := []struct{ code, want string }{
		{`ask()`, "options object required"},
		{`ask("yes?")`, "must be an object"},
		{`ask({message: "hi"})`, "type is required"},
		{`ask({type: "confirm"})`, "message is required"},
		{`ask({type: "magic", message: "hi"})`, "type must be"},
		{`ask({type: "choice", message: "which?"})`, "requires options"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			r := &answering{reply: true}
			s, events := askIn(r)
			_, err := s.Execute(tc.code)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.want)
			}
			if len(r.seen) != 0 {
				t.Errorf("the requester was called %d times for an invalid ask", len(r.seen))
			}
			if got := kinds(*events); len(got) != 0 {
				t.Errorf("an invalid ask reached the trace as %v", got)
			}
		})
	}
}

// A transport failure — the user went away, the stream closed — is the
// requester's error, reported to the script under ask's name and to the
// trace as an error, and never a zero value passed off as an answer.
func TestAskReportsTheRequesterError(t *testing.T) {
	r := &answering{err: errors.New("nobody is listening")}
	s, events := askIn(r)

	_, err := s.Execute(`ask({type: "confirm", message: "ok?"})`)
	if err == nil || !strings.Contains(err.Error(), "ask: nobody is listening") {
		t.Fatalf("got %v, want the requester's error under ask's name", err)
	}
	if got := kinds(*events); len(got) != 2 || got[0] != EventAsk || got[1] != EventError {
		t.Fatalf("events %v, want [ask error]", got)
	}
}
