package llm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A stream that keeps sending outlives the idle timeout; one that goes
// quiet is abandoned with ErrIdle, not left to the caller's deadline.
// The first is the slow host labs/codeloop kept losing to a wall-clock
// cap, the second the stalled one a cap was meant to catch.
func TestIdleTimeoutCutsSilenceNotSlowness(t *testing.T) {
	const idle = 150 * time.Millisecond
	frame := func(s string) string {
		return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", s)
	}
	serve := func(gaps []time.Duration) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, gap := range gaps {
				select {
				case <-time.After(gap):
				case <-r.Context().Done():
					return
				}
				_, _ = io.WriteString(w, frame("x"))
				w.(http.Flusher).Flush()
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		}))
	}
	req := CompletionRequest{Messages: []Message{{Role: "user", Content: "hi"}}}
	client := func(srv *httptest.Server) Client {
		c, err := NewOpenAI(Config{APIKey: "k", BaseURL: srv.URL, MaxRetries: -1, IdleTimeout: idle})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	slow := serve([]time.Duration{0, 100 * time.Millisecond, 100 * time.Millisecond, 100 * time.Millisecond})
	defer slow.Close()
	got, err := client(slow).Stream(context.Background(), req, nil)
	if err != nil || got.Content != "xxxx" {
		t.Fatalf("a stream that kept sending for longer than the idle timeout failed: %v %q", err, got.Content)
	}

	stalled := serve([]time.Duration{0, 2 * time.Second})
	defer stalled.Close()
	start := time.Now()
	_, err = client(stalled).Stream(context.Background(), req, nil)
	if !errors.Is(err, ErrIdle) {
		t.Fatalf("a stalled stream returned %v, want ErrIdle", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Errorf("the stalled stream was abandoned after %s, want about %s", waited, idle)
	}
}
