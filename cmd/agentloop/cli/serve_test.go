package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mind-vm/thinking-core/agentloop"
	"github.com/mind-vm/thinking-core/agentloopsql"
	"github.com/mind-vm/thinking-core/agentrun"
	"github.com/mind-vm/thinking-core/ext"
	"github.com/mind-vm/thinking-core/llm"
)

// --- config ---------------------------------------------------------------

func TestParseServeConfigDefaults(t *testing.T) {
	cfg, err := parseServeConfig(strings.NewReader(`{}`), "/etc/tc")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8080" || cfg.TokenEnv != "THINKING_CORE_TOKEN" {
		t.Errorf("defaults = %+v", cfg)
	}
	if cfg.Store != filepath.Join("/etc/tc", "thinking-core.db") {
		t.Errorf("a relative store resolves against the config's directory, got %s", cfg.Store)
	}
	if len(cfg.Capabilities) == 0 {
		t.Error("an omitted capabilities list means the defaults")
	}
}

func TestParseServeConfigRejects(t *testing.T) {
	for name, in := range map[string]string{
		"unknown field":       `{"listn": "127.0.0.1:1"}`,
		"unknown nested":      `{"policy": {"urlAllowPrefix": []}}`,
		"trailing data":       `{} {}`,
		"bad listen":          `{"listen": "nope"}`,
		"unknown capability":  `{"capabilities": ["teleport"]}`,
		"exec w/o workspace":  `{"capabilities": ["exec"]}`,
		"workspace w/o root":  `{"workspace": {}}`,
		"openapi w/o spec":    `{"openapi": [{"name": "x"}]}`,
		"duplicate openapi":   `{"openapi": [{"name": "x", "spec": "a"}, {"name": "x", "spec": "b"}]}`,
		"negative iterations": `{"maxIterations": -1}`,
		"mcp no transport":    `{"mcp": [{"name": "a"}]}`,
		"mcp both transports": `{"mcp": [{"name": "a", "command": ["x"], "url": "http://h"}]}`,
		"mcp bad name":        `{"mcp": [{"name": "a.b", "command": ["x"]}]}`,
		"mcp duplicate":       `{"mcp": [{"name": "a", "command": ["x"]}, {"name": "a", "url": "http://h"}]}`,
		"mcp env on url":      `{"mcp": [{"name": "a", "url": "http://h", "env": ["X"]}]}`,
		"mcp headers on cmd":  `{"mcp": [{"name": "a", "command": ["x"], "headersEnv": {"A": "B"}}]}`,
		"mcp unknown field":   `{"mcp": [{"name": "a", "command": ["x"], "ignoreReadOnly": true}]}`,
	} {
		if _, err := parseServeConfig(strings.NewReader(in), "."); err == nil {
			t.Errorf("%s: accepted %s", name, in)
		}
	}
}

func TestServeTokenRule(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, tc := range []struct {
		listen  string
		env     map[string]string
		wantErr bool
		wantTok string
		warns   bool
	}{
		{"127.0.0.1:1", map[string]string{"T": "s3"}, false, "s3", false},
		{"0.0.0.0:1", map[string]string{"T": "s3"}, false, "s3", false},
		{"127.0.0.1:1", nil, false, "", true},
		{"localhost:1", nil, false, "", true},
		{"[::1]:1", nil, false, "", true},
		{"0.0.0.0:1", nil, true, "", false},
		{":8080", nil, true, "", false},
		{"10.0.0.5:1", nil, true, "", false},
	} {
		tok, warn, err := serveToken(&serveConfig{Listen: tc.listen, TokenEnv: "T"}, env(tc.env))
		if (err != nil) != tc.wantErr || tok != tc.wantTok || (warn != "") != tc.warns {
			t.Errorf("%s env=%v: token=%q warn=%q err=%v", tc.listen, tc.env, tok, warn, err)
		}
	}
}

// --- server ---------------------------------------------------------------

type fakeLLM struct {
	mu   sync.Mutex
	resp []llm.CompletionResponse
}

func (f *fakeLLM) Name() string { return "fake" }
func (f *fakeLLM) Complete(context.Context, llm.CompletionRequest) (llm.CompletionResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.resp) == 0 {
		return llm.CompletionResponse{}, errors.New("script exhausted")
	}
	r := f.resp[0]
	f.resp = f.resp[1:]
	return r, nil
}
func (f *fakeLLM) Stream(ctx context.Context, req llm.CompletionRequest, on func(string)) (llm.CompletionResponse, error) {
	r, err := f.Complete(ctx, req)
	if err == nil && on != nil {
		on(r.Content)
	}
	return r, err
}

func script(code string) llm.CompletionResponse {
	return llm.CompletionResponse{Content: "```javascript\n" + code + "\n```"}
}

type testServe struct {
	*httptest.Server
	srv    *serveServer
	cancel context.CancelFunc
	// closeCaps is what shutdown runs; idempotent, so the test may call it
	// early to observe what it releases.
	closeCaps func()
}

func newTestServe(t *testing.T, token string, resp ...llm.CompletionResponse) *testServe {
	t.Helper()
	return newTestServeCfg(t, &serveConfig{Capabilities: []string{"ask"}}, func(string) string { return "" }, token, resp...)
}

func newTestServeCfg(t *testing.T, cfg *serveConfig, getenv func(string) string, token string, resp ...llm.CompletionResponse) *testServe {
	t.Helper()
	store, err := agentloopsql.Open(filepath.Join(t.TempDir(), "s.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	broker := agentrun.NewBroker()
	caps, closeCaps, err := serveCapabilities(cfg, nil, "", broker, getenv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := newServeServer(serveDeps{
		loop: agentloop.New(agentloop.Config{
			LLM: &fakeLLM{resp: resp}, Sessions: store, Steps: store,
			SandboxBuilder: &agentloop.DefaultSandboxBuilder{Capabilities: caps},
		}),
		store: store, broker: broker, token: token, ctx: ctx,
	})
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(func() {
		cancel()
		srv.wait()
		ts.Close()
		closeCaps()
		store.Close()
	})
	return &testServe{Server: ts, srv: srv, cancel: cancel, closeCaps: closeCaps}
}

func (ts *testServe) do(t *testing.T, method, path, token string, body any) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func (ts *testServe) start(t *testing.T, body map[string]any) (runID, sessionID string) {
	t.Helper()
	resp, out := ts.do(t, "POST", "/v1/runs", "", body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /v1/runs = %d %s", resp.StatusCode, out)
	}
	var v map[string]string
	_ = json.Unmarshal(out, &v)
	return v["runId"], v["sessionId"]
}

type sseEvent struct {
	Type string
	Data map[string]any
}

// readSSE reads a run's stream to its end and returns the events in order.
func (ts *testServe) readSSE(t *testing.T, runID string, onEvent func(sseEvent)) []sseEvent {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+"/v1/runs/"+runID+"/events", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	var evs []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur = sseEvent{Type: strings.TrimPrefix(line, "event: ")}
		case strings.HasPrefix(line, "data: "):
			_ = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &cur.Data)
			evs = append(evs, cur)
			if onEvent != nil {
				onEvent(cur)
			}
		}
	}
	return evs
}

func types(evs []sseEvent) []string {
	var out []string
	for _, e := range evs {
		out = append(out, e.Type)
	}
	return out
}

func countType(evs []sseEvent, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func TestServeRunStreamsExactlyOneDone(t *testing.T) {
	ts := newTestServe(t, "", script(`answer("42")`))
	runID, sessionID := ts.start(t, map[string]any{"message": "what is 6*7?"})

	evs := ts.readSSE(t, runID, nil)
	if got := countType(evs, "done"); got != 1 || evs[len(evs)-1].Type != "done" {
		t.Fatalf("want exactly one trailing done, got %v", types(evs))
	}
	if sid := evs[len(evs)-1].Data["summary"].(map[string]any)["sessionId"]; sid != sessionID {
		t.Errorf("done summary session = %v, want %s", sid, sessionID)
	}

	resp, out := ts.do(t, "GET", "/v1/runs/"+runID, "", nil)
	var v runView
	_ = json.Unmarshal(out, &v)
	if resp.StatusCode != 200 || v.Status != "completed" || v.Answer != "42" {
		t.Errorf("GET run = %d %s", resp.StatusCode, out)
	}
}

func TestServeLateSubscriberGetsTheWholeRun(t *testing.T) {
	ts := newTestServe(t, "", script(`log("x")`), script(`answer("done it")`))
	runID, _ := ts.start(t, map[string]any{"message": "go"})

	first := ts.readSSE(t, runID, nil) // the run is over once this returns
	late := ts.readSSE(t, runID, nil)  // a subscriber that arrives afterwards
	if strings.Join(types(first), ",") != strings.Join(types(late), ",") {
		t.Fatalf("replay differs:\nlive %v\nlate %v", types(first), types(late))
	}
	if countType(late, "done") != 1 || late[0].Type != "user" {
		t.Errorf("late replay must start at the user turn and end in one done: %v", types(late))
	}
}

func TestServeAskAnswerRoundTrip(t *testing.T) {
	ts := newTestServe(t, "", script(`var n = ask({type: "text", message: "name?"}); answer("hi " + n)`))
	runID, _ := ts.start(t, map[string]any{"message": "greet"})

	evs := ts.readSSE(t, runID, func(e sseEvent) {
		if e.Type != "input_request" {
			return
		}
		id := e.Data["args"].(map[string]any)["requestId"].(string)
		resp, out := ts.do(t, "POST", "/v1/runs/"+runID+"/answer", "", map[string]any{"requestId": id, "value": "Ada"})
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("answer = %d %s", resp.StatusCode, out)
		}
	})
	if countType(evs, "input_request") != 1 || countType(evs, "done") != 1 {
		t.Fatalf("events: %v", types(evs))
	}
	_, out := ts.do(t, "GET", "/v1/runs/"+runID, "", nil)
	var v runView
	_ = json.Unmarshal(out, &v)
	if v.Answer != "hi Ada" {
		t.Errorf("answer = %q (%s)", v.Answer, out)
	}

	// With nothing pending, an answer is a conflict, not a success.
	if resp, _ := ts.do(t, "POST", "/v1/runs/"+runID+"/answer", "", map[string]any{"requestId": "nope", "value": 1}); resp.StatusCode != http.StatusConflict {
		t.Errorf("stale answer = %d, want 409", resp.StatusCode)
	}
}

func TestServeWaitTrueReturnsTheResult(t *testing.T) {
	ts := newTestServe(t, "", script(`answer("sync")`))
	resp, out := ts.do(t, "POST", "/v1/runs?wait=true", "", map[string]any{"message": "go"})
	var v runView
	_ = json.Unmarshal(out, &v)
	if resp.StatusCode != 200 || v.Status != "completed" || v.Answer != "sync" || v.Summary == nil {
		t.Fatalf("wait=true = %d %s", resp.StatusCode, out)
	}
}

func TestServeFailedRunIsAnErrorStatusWithOneDone(t *testing.T) {
	ts := newTestServe(t, "") // the fake has nothing to say, so the first LLM call fails
	runID, _ := ts.start(t, map[string]any{"message": "go"})
	evs := ts.readSSE(t, runID, nil)
	if countType(evs, "done") != 1 || countType(evs, "error") != 1 {
		t.Fatalf("events: %v", types(evs))
	}
	_, out := ts.do(t, "GET", "/v1/runs/"+runID, "", nil)
	var v runView
	_ = json.Unmarshal(out, &v)
	if v.Status != "error" || v.Error == "" {
		t.Errorf("view = %s", out)
	}
}

func TestServeAuth(t *testing.T) {
	ts := newTestServe(t, "s3cret", script(`answer("x")`))

	for _, tok := range []string{"", "wrong", "s3cret-and-more"} {
		resp, _ := ts.do(t, "GET", "/v1/sessions", tok, nil)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("token %q = %d, want 401", tok, resp.StatusCode)
		}
	}
	if resp, _ := ts.do(t, "POST", "/v1/runs", "", map[string]any{"message": "x"}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST = %d", resp.StatusCode)
	}
	if resp, _ := ts.do(t, "GET", "/v1/sessions", "s3cret", nil); resp.StatusCode != 200 {
		t.Errorf("right token = %d", resp.StatusCode)
	}
	if resp, _ := ts.do(t, "GET", "/healthz", "", nil); resp.StatusCode != 200 {
		t.Errorf("/healthz needs no token, got %d", resp.StatusCode)
	}
}

func TestServeSessionsListAndDelete(t *testing.T) {
	ts := newTestServe(t, "", script(`answer("a")`))

	_, out := ts.do(t, "GET", "/v1/sessions", "", nil)
	if !strings.Contains(string(out), `"sessions":[]`) {
		t.Errorf("an empty list must be [], got %s", out)
	}

	resp, out := ts.do(t, "POST", "/v1/runs?wait=true", "", map[string]any{"message": "hello there"})
	var v runView
	_ = json.Unmarshal(out, &v)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}

	_, out = ts.do(t, "GET", "/v1/sessions", "", nil)
	var list struct{ Sessions []sessionView }
	_ = json.Unmarshal(out, &list)
	if len(list.Sessions) != 1 || list.Sessions[0].ID != v.SessionID || list.Sessions[0].Opening != "hello there" {
		t.Fatalf("list = %s", out)
	}

	if resp, _ := ts.do(t, "DELETE", "/v1/sessions/"+v.SessionID, "", nil); resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete = %d", resp.StatusCode)
	}
	if resp, _ := ts.do(t, "DELETE", "/v1/sessions/"+v.SessionID, "", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", resp.StatusCode)
	}
}

func TestServeOneRunPerSession(t *testing.T) {
	ts := newTestServe(t, "", script(`ask({type: "text", message: "hold"}); answer("x")`))
	runID, sessionID := ts.start(t, map[string]any{"message": "first"})

	// Wait for the run to be parked in ask().
	deadline := time.Now().Add(5 * time.Second)
	for {
		run := ts.srv.lookup(runID)
		run.mu.Lock()
		n := len(run.events)
		parked := false
		for _, e := range run.events {
			parked = parked || e.Type == "input_request"
		}
		run.mu.Unlock()
		if parked {
			break
		}
		if time.Now().After(deadline) || n < 0 {
			t.Fatal("run never reached ask()")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if resp, _ := ts.do(t, "POST", "/v1/runs", "", map[string]any{"message": "second", "sessionId": sessionID}); resp.StatusCode != http.StatusConflict {
		t.Errorf("second run on a busy session = %d, want 409", resp.StatusCode)
	}
	if resp, _ := ts.do(t, "DELETE", "/v1/sessions/"+sessionID, "", nil); resp.StatusCode != http.StatusConflict {
		t.Errorf("delete of a busy session = %d, want 409", resp.StatusCode)
	}
}

func TestServeRejectsBadRequests(t *testing.T) {
	ts := newTestServe(t, "")
	for name, body := range map[string]any{
		"no message":       map[string]any{},
		"blank message":    map[string]any{"message": "  "},
		"unknown field":    map[string]any{"message": "x", "mesage": "y"},
		"non-uuid session": map[string]any{"message": "x", "sessionId": "my-session"},
	} {
		if resp, _ := ts.do(t, "POST", "/v1/runs", "", body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, resp.StatusCode)
		}
	}
	if resp, _ := ts.do(t, "GET", "/v1/runs/nope", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown run = %d", resp.StatusCode)
	}
}

func TestServeCancelledRunsEndWithADone(t *testing.T) {
	ts := newTestServe(t, "", script(`ask({type: "text", message: "hold"}); answer("x")`))
	runID, _ := ts.start(t, map[string]any{"message": "go"})

	done := make(chan []sseEvent, 1)
	go func() { done <- ts.readSSE(t, runID, nil) }()
	time.Sleep(100 * time.Millisecond)
	ts.cancel() // what a SIGTERM does

	select {
	case evs := <-done:
		if countType(evs, "done") != 1 {
			t.Errorf("events: %v", types(evs))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not end after the run was cancelled")
	}
	if v := ts.srv.lookup(runID).view(); v.Status != "canceled" {
		t.Errorf("status = %s", v.Status)
	}
}

// --- mcp --------------------------------------------------------------------

func TestParseServeConfigMCP(t *testing.T) {
	cfg, err := parseServeConfig(strings.NewReader(`{"mcp": [
		{"name": "fs", "command": ["npx", "-y", "srv"], "env": ["HOME_TOKEN"], "ignoreReadOnlyHint": true},
		{"name": "gh", "url": "https://x/mcp", "headersEnv": {"Authorization": "GH_AUTH"}}]}`), ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCP) != 2 || !cfg.MCP[0].IgnoreReadOnlyHint || cfg.MCP[1].HeadersEnv["Authorization"] != "GH_AUTH" {
		t.Errorf("mcp = %+v", cfg.MCP)
	}
}

type fakeMCPSession struct{}

func (fakeMCPSession) ListTools(context.Context) ([]ext.MCPTool, error) {
	return []ext.MCPTool{{Name: "echo", Description: "echo text", ReadOnly: true}}, nil
}
func (fakeMCPSession) CallTool(_ context.Context, _ string, args map[string]any) (ext.MCPResult, error) {
	return ext.MCPResult{Text: "echo:" + args["text"].(string)}, nil
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

// setHooks swaps the package-level hooks Main would set.
func setHooks(t *testing.T, h Hooks) {
	t.Helper()
	old := hooks
	hooks = h
	t.Cleanup(func() { hooks = old })
}

func TestServeMCPRefusedByStockBuild(t *testing.T) {
	setHooks(t, Hooks{})
	cfg := &serveConfig{MCP: []serveConfigMCP{{Name: "fake", Command: []string{"x"}}}}
	_, _, err := serveCapabilities(cfg, nil, "", agentrun.NewBroker(), func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "MCP SDK") {
		t.Errorf("err = %v, want a pointer to the ext/mcp binary", err)
	}
}

func TestServeMCPRunThroughFakeHook(t *testing.T) {
	var got MCPSpec
	closed := false
	setHooks(t, Hooks{OpenMCP: func(_ context.Context, spec MCPSpec) (ext.MCPSession, io.Closer, error) {
		got = spec
		return fakeMCPSession{}, closerFunc(func() error { closed = true; return nil }), nil
	}})
	cfg := &serveConfig{Capabilities: []string{}, MCP: []serveConfigMCP{{
		Name: "fake", URL: "https://x/mcp", HeadersEnv: map[string]string{"Authorization": "FAKE_AUTH"},
	}}}
	env := func(k string) string {
		if k == "FAKE_AUTH" {
			return "Bearer s"
		}
		return ""
	}
	ts := newTestServeCfg(t, cfg, env, "", script(`answer(require("mcp/fake").echo({text: "hi"}))`))
	resp, out := ts.do(t, "POST", "/v1/runs?wait=true", "", map[string]any{"message": "go"})
	var v runView
	_ = json.Unmarshal(out, &v)
	if resp.StatusCode != 200 || v.Answer != "echo:hi" {
		t.Fatalf("run = %d %s", resp.StatusCode, out)
	}
	if got.Headers["Authorization"] != "Bearer s" || got.URL != "https://x/mcp" {
		t.Errorf("spec handed to the hook = %+v", got)
	}
	ts.closeCaps()
	if !closed {
		t.Error("the MCP session must be closed on shutdown")
	}
}

func TestServeMCPMissingHeaderEnvFailsStartup(t *testing.T) {
	setHooks(t, Hooks{OpenMCP: func(context.Context, MCPSpec) (ext.MCPSession, io.Closer, error) {
		t.Error("the server must not be opened without its secret")
		return nil, nil, nil
	}})
	cfg := &serveConfig{MCP: []serveConfigMCP{{Name: "g", URL: "https://x", HeadersEnv: map[string]string{"Authorization": "NOPE"}}}}
	if _, _, err := serveCapabilities(cfg, nil, "", agentrun.NewBroker(), func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "$NOPE") {
		t.Errorf("err = %v", err)
	}
}
