package pool_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mind-vm/thinking-core/agentloop"
	"github.com/mind-vm/thinking-core/pool"
)

// A pooled sandbox outlives the Run that cancelled it, so the interrupt
// must not leak into the next Run of the same session.
func TestSandboxPool_ReusableAfterACancelledTurn(t *testing.T) {
	p := pool.New(&fakeBuilder{}, pool.Options{})
	defer p.Close()
	sess := agentloop.Session{ID: "s1"}

	sb, release, err := p.Build(context.Background(), sess, agentloop.Scope{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel)
	if _, _, err := sb.ExecuteTurnContext(ctx, `while (true) {}`, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	release()

	sb2, release2, err := p.Build(context.Background(), sess, agentloop.Scope{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	if sb2 != sb {
		t.Fatal("expected the pooled sandbox")
	}
	_, ret, err := sb2.ExecuteTurn(`state.ok = 1`, nil)
	if err != nil || string(ret) != `{"ok":1}` {
		t.Fatalf("after a cancel: ret = %s, err = %v", ret, err)
	}
}
