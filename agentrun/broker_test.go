package agentrun_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mind-vm/thinking-core/agentrun"
	"github.com/mind-vm/thinking-core/sandbox"
)

// asked starts an ask() on its own goroutine, the way the sandbox blocks on
// it, and returns the requestId the broker announced and a channel with the
// outcome.
func asked(t *testing.T, b *agentrun.Broker, session uuid.UUID, events <-chan agentrun.Event) (string, <-chan error, <-chan any) {
	t.Helper()
	errc, valc := make(chan error, 1), make(chan any, 1)
	go func() {
		v, err := b.Requester(session).Request(sandbox.InputRequestData{InputType: "confirm", Message: "Proceed?"})
		errc <- err
		valc <- v
	}()
	select {
	case e := <-events:
		if e.Type != "input_request" || e.Content != "Proceed?" {
			t.Fatalf("announced %+v", e)
		}
		id, _ := e.Args["requestId"].(string)
		if id == "" {
			t.Fatal("input_request carries no requestId to reply with")
		}
		return id, errc, valc
	case <-time.After(5 * time.Second):
		t.Fatal("ask() never reached the stream")
	}
	return "", nil, nil
}

func sink() (func(agentrun.Event), chan agentrun.Event) {
	ch := make(chan agentrun.Event, 8)
	return func(e agentrun.Event) { ch <- e }, ch
}

func TestBroker_ReplyResolvesTheAsk(t *testing.T) {
	b := agentrun.NewBroker()
	session, org := uuid.New(), uuid.New()
	emit, events := sink()
	end := b.Begin(session, org, emit)
	defer end()

	id, errc, valc := asked(t, b, session, events)
	if err := b.Reply(session, org, id, true); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if v := <-valc; v != true {
		t.Fatalf("ask() returned %v", v)
	}
	if err := b.Reply(session, org, id, true); err == nil {
		t.Fatal("a second reply to the same prompt was accepted")
	}
}

// Another tenant's reply must not resolve the prompt, and must not learn
// that the session exists either.
func TestBroker_ReplyFromAnotherTenantIsNoLiveRun(t *testing.T) {
	b := agentrun.NewBroker()
	session, org := uuid.New(), uuid.New()
	emit, events := sink()
	end := b.Begin(session, org, emit)
	defer end()

	id, _, _ := asked(t, b, session, events)
	err := b.Reply(session, uuid.New(), id, true)
	if err == nil {
		t.Fatal("another tenant resolved the prompt")
	}
	if want := b.Reply(uuid.New(), org, id, true); want == nil || err.Error() != want.Error() {
		t.Fatalf("the wrong tenant got %q, an unknown session %q; they must read the same", err, want)
	}
}

func TestBroker_EndingTheRunUnblocksTheAsk(t *testing.T) {
	b := agentrun.NewBroker()
	session := uuid.New()
	emit, events := sink()
	end := b.Begin(session, uuid.New(), emit)

	_, errc, _ := asked(t, b, session, events)
	end()
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "ended") {
			t.Fatalf("ask() after end = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ask() still blocked after its run ended")
	}
}

func TestBroker_AskWithoutALiveRunFails(t *testing.T) {
	if _, err := agentrun.NewBroker().Requester(uuid.New()).Request(sandbox.InputRequestData{}); err == nil {
		t.Fatal("ask() with no live run succeeded")
	}
}

// The newest stream owns the prompts; the old one's end must not drop it.
func TestBroker_ASecondBeginOwnsTheSession(t *testing.T) {
	b := agentrun.NewBroker()
	session, org := uuid.New(), uuid.New()
	emitOld, _ := sink()
	endOld := b.Begin(session, org, emitOld)
	emitNew, events := sink()
	endNew := b.Begin(session, org, emitNew)
	defer endNew()
	endOld()

	id, errc, _ := asked(t, b, session, events)
	if err := b.Reply(session, org, id, "yes"); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}
