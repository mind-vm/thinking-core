package reliability

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

// Chat is a bare OpenAI-wire client, shared by every arm the bench drives
// itself (A–E), so they differ in nothing but how they use the model: the
// same request, the same retries, the same timeouts. The agentloop arm uses
// the library's own client instead, because that is part of what it
// measures.
type Chat struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Msg is one chat message. Content is any so an assistant message with
// tool calls can carry null, which some providers require.
type Msg struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type Reply struct {
	Content          string
	ToolCalls        []ToolCall
	FinishReason     string
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
}

// Model is one model under test, with the list price it is costed at.
type Model struct {
	Name            string  // short name for the report
	ID              string  // the gateway's model id
	InPerM, OutPerM float64 // USD per million tokens
	ReasoningEffort string  // sent as reasoning_effort when set
}

func (m Model) Cost(prompt, completion int) float64 {
	return (float64(prompt)*m.InPerM + float64(completion)*m.OutPerM) / 1e6
}

// MaxCompletionTokens leaves room for a reasoning model's thinking: a
// tighter cap cut replies off mid-thought and read as an empty turn.
const MaxCompletionTokens = 4096

var errRetryable = errors.New("retryable")

func (c *Chat) Complete(ctx context.Context, m Model, msgs []Msg, tools []map[string]any) (Reply, error) {
	body := map[string]any{"model": m.ID, "messages": msgs, "max_tokens": MaxCompletionTokens}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if m.ReasoningEffort != "" {
		body["reasoning_effort"] = m.ReasoningEffort
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Reply{}, err
	}
	var last error
	for attempt := range 4 {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Reply{}, ctx.Err()
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		r, err := c.once(ctx, raw)
		if err == nil {
			return r, nil
		}
		last = err
		if !errors.Is(err, errRetryable) {
			break
		}
	}
	return Reply{}, last
}

func (c *Chat) once(ctx context.Context, raw []byte) (Reply, error) {
	cctx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return Reply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Reply{}, ctx.Err()
		}
		return Reply{}, fmt.Errorf("%w: %v", errRetryable, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(data), 300))
		if resp.StatusCode == 408 || resp.StatusCode == 409 || resp.StatusCode == 425 || resp.StatusCode == 429 || resp.StatusCode >= 500 {
			err = fmt.Errorf("%w: %v", errRetryable, err)
		}
		return Reply{}, err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   *string    `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens            int `json:"prompt_tokens"`
			CompletionTokens        int `json:"completion_tokens"`
			CompletionTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Reply{}, fmt.Errorf("%w: bad JSON: %v", errRetryable, err)
	}
	if len(out.Choices) == 0 {
		return Reply{}, fmt.Errorf("%w: no choices: %s", errRetryable, truncate(string(data), 300))
	}
	ch := out.Choices[0]
	r := Reply{
		ToolCalls:        ch.Message.ToolCalls,
		FinishReason:     ch.FinishReason,
		PromptTokens:     out.Usage.PromptTokens,
		CompletionTokens: out.Usage.CompletionTokens,
		ReasoningTokens:  out.Usage.CompletionTokensDetails.ReasoningTokens,
	}
	if ch.Message.Content != nil {
		r.Content = *ch.Message.Content
	}
	return r, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
