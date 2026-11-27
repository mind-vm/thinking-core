package bench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ToolCalling is the baseline: the plain native function-calling loop
// every provider documents. The fixture tools go in `tools`, the model
// answers with `tool_calls`, each result is serialised as JSON into a
// role "tool" message, and the run ends on the first reply without tool
// calls. Parallel tool calls are taken as the model emits them.
//
// It is written to be the loop a careful engineer would ship, not a straw
// man: errors go back to the model as tool results so it can recover,
// transient HTTP failures are retried, and nothing is truncated — a cap
// on tool output would be a handicap (missing rows) or an aid (fewer
// tokens) that the other side does not get.
//
// llm.Client has no tool-calling surface (the library's own protocol is
// text, ADR-0004), so this carries the minimal wire types it needs.
type ToolCalling struct {
	Endpoint Endpoint
}

func (ToolCalling) Name() string { return "tool-calling" }

func (a ToolCalling) Run(ctx context.Context, model string, d *Data, task Task) Outcome {
	ctx, cancel := context.WithTimeout(ctx, a.Endpoint.runTimeout())
	defer cancel()

	tools := Tools()
	wireTools := make([]wireTool, 0, len(tools))
	byName := map[string]Tool{}
	for _, t := range tools {
		wireTools = append(wireTools, wireTool{Type: "function", Function: wireFunction{Name: t.Name, Description: t.Description, Parameters: t.Schema}})
		byName[t.Name] = t
	}

	msgs := []wireMessage{
		{Role: "system", Content: ptr(Persona)},
		{Role: "user", Content: ptr(task.Prompt)},
	}
	var out Outcome
	for out.Turns < a.Endpoint.maxTurns() {
		resp, err := a.complete(ctx, wireRequest{Model: model, Messages: msgs, Tools: wireTools})
		out.Turns++
		if err != nil {
			out.Err = err
			return out
		}
		if resp.Usage != nil {
			out.PromptTokens += resp.Usage.PromptTokens
			out.CompletionTokens += resp.Usage.CompletionTokens
		}
		if len(resp.Choices) == 0 {
			out.Err = errors.New("response has no choices")
			return out
		}
		msg := resp.Choices[0].Message
		if len(msg.ToolCalls) == 0 {
			if msg.Content != nil {
				out.Final = *msg.Content
			}
			return out
		}
		for i := range msg.ToolCalls {
			// Echoed back verbatim except for a missing type, which
			// stricter hosts reject on the next request.
			if msg.ToolCalls[i].Type == "" {
				msg.ToolCalls[i].Type = "function"
			}
		}
		msgs = append(msgs, wireMessage{Role: "assistant", Content: msg.Content, ToolCalls: msg.ToolCalls})
		for _, call := range msg.ToolCalls {
			out.ToolCalls++
			msgs = append(msgs, wireMessage{Role: "tool", ToolCallID: call.ID, Content: ptr(runTool(byName, d, call))})
		}
	}
	out.Err = fmt.Errorf("no final answer within %d turns", a.Endpoint.maxTurns())
	return out
}

// runTool returns what the model reads back: the result as compact JSON,
// or {"error": ...} — the usual convention, and the one that lets the
// model fix an argument and retry instead of the run failing.
func runTool(byName map[string]Tool, d *Data, call wireToolCall) string {
	errJSON := func(err error) string {
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		return string(b)
	}
	t, ok := byName[call.Function.Name]
	if !ok {
		return errJSON(fmt.Errorf("unknown tool %q", call.Function.Name))
	}
	args := map[string]any{}
	if s := strings.TrimSpace(call.Function.Arguments); s != "" {
		if err := json.Unmarshal([]byte(s), &args); err != nil {
			return errJSON(fmt.Errorf("arguments are not a JSON object: %v", err))
		}
	}
	v, err := t.Call(d, args)
	if err != nil {
		return errJSON(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return errJSON(err)
	}
	return string(b)
}

func (a ToolCalling) complete(ctx context.Context, req wireRequest) (*wireResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	hc := a.Endpoint.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	url := strings.TrimRight(a.Endpoint.BaseURL, "/") + "/chat/completions"
	var lastErr error
	// Three retries with doubling backoff on transport errors, 429 and
	// 5xx — what llm.NewOpenAI does for the other side by default.
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(500<<(attempt-1)) * time.Millisecond):
			}
		}
		hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		hreq.Header.Set("Content-Type", "application/json")
		if a.Endpoint.APIKey != "" {
			hreq.Header.Set("Authorization", "Bearer "+a.Endpoint.APIKey)
		}
		hresp, err := hc.Do(hreq)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		raw, err := io.ReadAll(hresp.Body)
		_ = hresp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if hresp.StatusCode == http.StatusTooManyRequests || hresp.StatusCode >= 500 {
			lastErr = fmt.Errorf("status %d: %s", hresp.StatusCode, strings.TrimSpace(string(raw)))
			continue
		}
		if hresp.StatusCode >= 300 {
			return nil, fmt.Errorf("status %d: %s", hresp.StatusCode, strings.TrimSpace(string(raw)))
		}
		var out wireResponse
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
		return &out, nil
	}
	return nil, lastErr
}

func ptr(s string) *string { return &s }

// The OpenAI-wire tool-calling shapes. Content is a pointer because an
// assistant message carrying tool_calls has content null, and some hosts
// reject an empty string there.
type wireRequest struct {
	Model    string        `json:"model"`
	Messages []wireMessage `json:"messages"`
	Tools    []wireTool    `json:"tools,omitempty"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    *string        `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireResponse struct {
	Choices []struct {
		Message struct {
			Content   *string        `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}
