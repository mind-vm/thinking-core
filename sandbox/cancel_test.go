package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The exec timeout alone would let a CPU-bound script outlive a client that
// went away for up to two minutes.
func TestExecuteTurnContextInterruptsOnCancel(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	_, ret, err := s.ExecuteTurnContext(ctx, `while (true) {}`, json.RawMessage(`{"keep":1}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if ret != nil {
		t.Errorf("ret = %s, want nil so the previous state is kept", ret)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("cancellation took %s", elapsed)
	}
}

func TestExecuteTurnContextReportsDeadline(t *testing.T) {
	s := New()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := s.ExecuteTurnContext(ctx, `while (true) {}`, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestExecuteTurnContextAlreadyCancelledRunsNothing(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.ExecuteTurnContext(ctx, `state.ran = true`, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// A leftover watcher would Interrupt a runtime a later turn is using.
func TestExecuteTurnContextLeavesNoWatcherGoroutine(t *testing.T) {
	s := New()
	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		if _, _, err := s.ExecuteTurnContext(ctx, `state.n = 1`, nil); err != nil {
			t.Fatal(err)
		}
		cancel()
	}
	// Cancelling after the turn must not poison the next one either.
	if _, ret := turn(t, s, `state.n = 2`); string(ret) != `{"n":2}` {
		t.Errorf("ret = %s", ret)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		t.Errorf("goroutines %d -> %d: a watcher leaked", before, after)
	}
}

func TestSandboxIsUsableAfterACancel(t *testing.T) {
	s := New()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	if _, _, err := s.ExecuteTurnContext(ctx, `while (true) {}`, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if _, ret := turn(t, s, `state.ok = true`); string(ret) != `{"ok":true}` {
		t.Errorf("ret = %s", ret)
	}
}

func TestStateOverTheLimitIsAScriptError(t *testing.T) {
	s := New()
	s.SetMaxStateBytes(100)
	_, ret, err := s.ExecuteTurn(`state.big = "x".repeat(500)`, json.RawMessage(`{"a":1}`))
	if err == nil {
		t.Fatal("want an error for oversized state")
	}
	for _, want := range []string{"state is", "100 byte limit", "keep less"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if ret != nil {
		t.Errorf("ret = %s, want nil so the previous state is kept", ret)
	}
	if _, ret := turn(t, s, `state.small = 1`); string(ret) != `{"small":1}` {
		t.Errorf("a sandbox that refused big state must still run: %s", ret)
	}
}

func TestStateLimitDefaultAndUnlimited(t *testing.T) {
	big := `state.big = "x".repeat(` + strconv.Itoa(DefaultMaxStateBytes+10) + `)`
	if _, _, err := New().ExecuteTurn(big, nil); err == nil {
		t.Error("zero should mean the default cap")
	}
	s := New()
	s.SetMaxStateBytes(-1)
	if _, _, err := s.ExecuteTurn(big, nil); err != nil {
		t.Errorf("negative should mean unlimited: %v", err)
	}
}
