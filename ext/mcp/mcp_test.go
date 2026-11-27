package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mind-vm/thinking-core/ext"
	"github.com/mind-vm/thinking-core/ext/mcp"
	"github.com/mind-vm/thinking-core/sandbox"
)

// newTestServer is a tiny MCP server built with the same SDK as the
// client, so the test exercises the real wire protocol rather than a
// stand-in for it.
func newTestServer() *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "test", Version: "v0"}, nil)
	schema := func(props map[string]any, required ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		return m
	}
	text := func(s string) *sdk.CallToolResult {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: s}}}
	}
	args := func(req *sdk.CallToolRequest) map[string]any {
		var m map[string]any
		_ = json.Unmarshal(req.Params.Arguments, &m)
		return m
	}

	s.AddTool(&sdk.Tool{
		Name: "echo_text", Description: "Echo the text back.\nMore detail.",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		InputSchema: schema(map[string]any{"text": map[string]any{"type": "string"}}, "text"),
	}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return text(args(req)["text"].(string)), nil
	})
	s.AddTool(&sdk.Tool{
		Name: "add", Description: "Add two numbers",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		InputSchema: schema(map[string]any{"a": map[string]any{"type": "number"}, "b": map[string]any{"type": "number"}}, "a", "b"),
	}, func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		a := args(req)
		sum := a["a"].(float64) + a["b"].(float64)
		r := text("sum is shown in structured content")
		r.StructuredContent = map[string]any{"sum": sum}
		return r, nil
	})
	s.AddTool(&sdk.Tool{
		Name: "boom", Description: "Always fails", InputSchema: schema(map[string]any{}),
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		r := text("it broke: disk full")
		r.IsError = true
		return r, nil
	})
	s.AddTool(&sdk.Tool{
		Name: "list_items", Description: "A JSON text result", Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		InputSchema: schema(map[string]any{}),
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return text(`[{"id":1},{"id":2}]`), nil
	})
	s.AddTool(&sdk.Tool{
		Name: "env_probe", Description: "Report which probe variables the server process sees",
		Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true}, InputSchema: schema(map[string]any{}),
	}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return text("secret=" + os.Getenv("SECRET_PROBE") + " allowed=" + os.Getenv("ALLOWED_PROBE")), nil
	})
	return s
}

// TestMain lets the test binary double as a stdio MCP server: ConnectStdio
// needs a real executable, and the binary under test is one.
func TestMain(m *testing.M) {
	if os.Getenv("MCP_TEST_SERVER") == "1" {
		if err := newTestServer().Run(context.Background(), &sdk.StdioTransport{}); err != nil {
			os.Exit(1)
		}
		return
	}
	os.Exit(m.Run())
}

func connectInMemory(t *testing.T) *mcp.Session {
	t.Helper()
	ctx := context.Background()
	st, ct := sdk.NewInMemoryTransports()
	if _, err := newTestServer().Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "c", Version: "v0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess := mcp.NewSession(cs)
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func TestSession_ListAndCall(t *testing.T) {
	ctx := context.Background()
	sess := connectInMemory(t)

	tools, err := sess.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]ext.MCPTool{}
	for _, tl := range tools {
		byName[tl.Name] = tl
	}
	if len(tools) != 5 || !byName["echo_text"].ReadOnly || byName["boom"].ReadOnly {
		t.Errorf("tools = %+v", tools)
	}
	if props, _ := byName["add"].InputSchema["properties"].(map[string]any); len(props) != 2 {
		t.Errorf("the input schema did not arrive as a map: %v", byName["add"].InputSchema)
	}

	res, err := sess.CallTool(ctx, "echo_text", map[string]any{"text": "hi"})
	if err != nil || res.Text != "hi" || res.IsError {
		t.Errorf("echo = %+v, %v", res, err)
	}
	res, err = sess.CallTool(ctx, "add", map[string]any{"a": 2, "b": 3})
	if err != nil || res.Structured.(map[string]any)["sum"] != 5.0 {
		t.Errorf("add = %+v, %v", res, err)
	}
	res, err = sess.CallTool(ctx, "boom", nil)
	if err != nil || !res.IsError || !strings.Contains(res.Text, "disk full") {
		t.Errorf("boom = %+v, %v", res, err)
	}
}

// TestMCPPack_EndToEnd runs a script against a real MCP server through the
// real pack: the whole path from require() to the wire and back.
func TestMCPPack_EndToEnd(t *testing.T) {
	ctx := context.Background()
	sess := connectInMemory(t)
	pack, err := ext.MCPPack(ctx, "demo", sess, ext.MCPConfig{})
	if err != nil {
		t.Fatal(err)
	}
	sb := sandbox.New(sandbox.RequirePack(ctx), pack)
	sb.SetPolicy(sandbox.DefaultPolicy{AllowTools: []string{"mcp.demo.boom"}})

	out, err := sb.Execute(`
		var out = (function () {
			var demo = require("mcp/demo");
			var sum = demo.add({a: 20, b: 22}).sum;
			var items = demo.listItems();
			var echoed = demo.echoText({text: "n=" + items.length});
			var failure;
			try { demo.boom(); } catch (e) { failure = e.message; }
			return [sum, echoed, failure].join(" | ");
		})();
		out`)
	if err != nil {
		t.Fatal(err)
	}
	if want := "42 | n=2 | it broke: disk full"; out != want {
		t.Errorf("got %q, want %q", out, want)
	}
}

func TestConnectStdio(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_TEST_SERVER", "1")
	t.Setenv("SECRET_PROBE", "leaked")
	t.Setenv("ALLOWED_PROBE", "granted")

	sess, err := mcp.ConnectStdio(context.Background(), mcp.StdioConfig{
		Command: self,
		// MCP_TEST_SERVER must reach the child for it to act as a server.
		Env: []string{"MCP_TEST_SERVER", "ALLOWED_PROBE"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	res, err := sess.CallTool(context.Background(), "env_probe", nil)
	if err != nil {
		t.Fatal(err)
	}
	// The credential-shaped variable stays home; the named one goes through.
	if res.Text != "secret= allowed=granted" {
		t.Errorf("child environment: %q", res.Text)
	}
}

func TestConnectStdio_MissingCommand(t *testing.T) {
	if _, err := mcp.ConnectStdio(context.Background(), mcp.StdioConfig{Command: "/nonexistent/mcp-server"}); err == nil {
		t.Error("expected an error for a command that does not exist")
	}
	if _, err := mcp.ConnectStdio(context.Background(), mcp.StdioConfig{}); err == nil {
		t.Error("expected an error for no command")
	}
}

func TestConnectHTTP_SendsHeaders(t *testing.T) {
	var mu sync.Mutex
	var auths []string
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return newTestServer() }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auths = append(auths, r.Header.Get("Authorization"))
		mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	defer ts.Close()

	sess, err := mcp.ConnectHTTP(context.Background(), mcp.HTTPConfig{
		URL:     ts.URL,
		Headers: map[string]string{"Authorization": "Bearer t0ken"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	res, err := sess.CallTool(context.Background(), "echo_text", map[string]any{"text": "over http"})
	if err != nil || res.Text != "over http" {
		t.Fatalf("echo = %+v, %v", res, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(auths) == 0 {
		t.Fatal("no requests reached the server")
	}
	for _, a := range auths {
		if a != "Bearer t0ken" {
			t.Errorf("a request went out with Authorization %q", a)
		}
	}
}
