package llm

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Thinking and the stop reason arrive in different fields depending on
// the upstream; both shapes, streamed and not, must land in the response.
func TestReasoningAndFinishReasonAreCaptured(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think \"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"more\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"length\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":30,\"completion_tokens_details\":{\"reasoning_tokens\":28}}}\n\n" +
		"data: [DONE]\n\n"
	complete := `{"choices":[{"message":{"content":"","reasoning":"hidden plan"},"finish_reason":"content_filter"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, stream)
			return
		}
		_, _ = io.WriteString(w, complete)
	}))
	defer srv.Close()
	c := testClient(t, srv, -1)
	req := CompletionRequest{Messages: []Message{{Role: "user", Content: "hi"}}}

	got, err := c.Stream(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "hi" || got.Reasoning != "think more" || got.FinishReason != "length" || got.ReasoningTokens != 28 || got.OutputTokens != 30 {
		t.Errorf("stream = %+v", got)
	}

	got, err = c.Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Reasoning != "hidden plan" || got.FinishReason != "content_filter" || got.ReasoningTokens != 0 {
		t.Errorf("complete = %+v", got)
	}
}
