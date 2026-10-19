package agentrun_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mind-vm/thinking-core/agentrun"
)

// noFlush is a ResponseWriter that cannot stream.
type noFlush struct{ http.ResponseWriter }

func TestOpen_RefusesAWriterThatCannotFlush(t *testing.T) {
	rec := httptest.NewRecorder()
	if s := agentrun.Open(noFlush{rec}); s != nil {
		t.Fatal("Open returned a stream over a writer that cannot flush")
	}
	if rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
		t.Fatal("Open wrote to a writer it then refused; the caller can no longer send an error status")
	}
}

func TestOpen_WritesStreamHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	if agentrun.Open(rec) == nil {
		t.Fatal("Open refused a flushing writer")
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Errorf("X-Accel-Buffering = %q; a buffering proxy would hold the whole run", got)
	}
	if !rec.Flushed {
		t.Error("headers were not flushed; the client waits for the first event to learn the stream is open")
	}
}

func TestSend_FramesOneEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	agentrun.Open(rec).Send(agentrun.Event{Type: "response", Content: "hi"})
	want := "event: response\ndata: {\"type\":\"response\",\"content\":\"hi\"}\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("frame = %q, want %q", got, want)
	}
}

func TestFinish_AfterTheLoopsDone_AddsNothing(t *testing.T) {
	rec := httptest.NewRecorder()
	s := agentrun.Open(rec)
	s.Send(agentrun.Event{Type: "done", Summary: &agentrun.Summary{SessionID: "s1"}})
	s.Finish(agentrun.Summary{SessionID: "s1"}, nil)
	if n := strings.Count(rec.Body.String(), "event: done\n"); n != 1 {
		t.Fatalf("%d done events, want exactly one", n)
	}
}

func TestFinish_WithoutTheLoopsDone_SendsOne(t *testing.T) {
	rec := httptest.NewRecorder()
	agentrun.Open(rec).Finish(agentrun.Summary{SessionID: "s1"}, nil)
	if n := strings.Count(rec.Body.String(), "event: done\n"); n != 1 {
		t.Fatalf("%d done events, want exactly one", n)
	}
}

func TestFinish_WithAnError_SendsErrorThenDone(t *testing.T) {
	rec := httptest.NewRecorder()
	agentrun.Open(rec).Finish(agentrun.Summary{}, errors.New("model unreachable"))
	body := rec.Body.String()
	i, j := strings.Index(body, "event: error\n"), strings.Index(body, "event: done\n")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("want an error event then a done event, got %q", body)
	}
	if !strings.Contains(body, "model unreachable") {
		t.Fatalf("the error's text did not reach the client: %q", body)
	}
}

// The loop emits from its own goroutines. Run with -race.
func TestSend_IsSafeForConcurrentUse(t *testing.T) {
	rec := httptest.NewRecorder()
	s := agentrun.Open(rec)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Send(agentrun.Event{Type: "sandbox_event", Content: "x"})
		}()
	}
	wg.Wait()
	if n := strings.Count(rec.Body.String(), "event: sandbox_event\ndata: "); n != 20 {
		t.Fatalf("%d intact frames, want 20", n)
	}
}
