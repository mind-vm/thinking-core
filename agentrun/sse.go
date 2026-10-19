package agentrun

import (
	"encoding/json"
	"net/http"
	"sync"
)

// sse.go serves a run as Server-Sent Events.
//
// A run is the one operation in an agent API that is not a request with a
// body: it is a sequence of turns that can take minutes, and watching the
// model write JavaScript and get a result back is the product. A client that
// had to wait for a transcript would be watching a spinner during the only
// interesting part.
//
// The stream is one-way. The reply half of ask() arrives on a separate POST
// and meets the blocked run through the Broker; see broker.go.

// Stream writes a run's events to one client.
type Stream struct {
	w       http.ResponseWriter
	flusher http.Flusher

	// The loop emits from its own goroutines and an http.ResponseWriter is
	// not safe for concurrent use. One mutex is the whole synchronisation
	// story; a channel and a pump would buy nothing, because the loop already
	// blocks on whatever the sandbox is doing.
	mu      sync.Mutex
	sawDone bool
}

// Open writes the event-stream headers and returns the stream, or nil when
// the writer cannot flush — in which case nothing has been written and the
// caller should answer with an ordinary error status.
func Open(w http.ResponseWriter) *Stream {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// Proxies that buffer would defeat the point of the whole endpoint.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &Stream{w: w, flusher: flusher}
}

// Send writes one event and flushes.
func (s *Stream) Send(e Event) {
	payload, err := json.Marshal(e)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Type == "done" {
		s.sawDone = true
	}
	_, _ = s.w.Write([]byte("event: " + e.Type + "\ndata: "))
	_, _ = s.w.Write(payload)
	_, _ = s.w.Write([]byte("\n\n"))
	s.flusher.Flush()
}

// Finish closes the stream with exactly one terminal event.
//
// The loop emits its own `done`, and a handler has to emit one on the paths
// the loop never reaches — a failure before or during the run. Without the
// bookkeeping a normal run ends with two, and a client that stops reading on
// `done` would be right to be confused.
//
// A non-nil err becomes an `error` event first: the stream is already open, so
// a failure is an event rather than a status, and a client that has been
// rendering turns for two minutes needs to be told what happened rather than
// handed a status code it cannot see.
func (s *Stream) Finish(summary Summary, err error) {
	if err != nil {
		s.Send(Event{Type: "error", Content: err.Error()})
		s.Send(Event{Type: "done", Summary: &summary})
		return
	}
	s.mu.Lock()
	emitted := s.sawDone
	s.mu.Unlock()
	if !emitted {
		s.Send(Event{Type: "done", Summary: &summary})
	}
}
