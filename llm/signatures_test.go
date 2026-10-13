package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

// Gemini 3's thought signatures arrive in provider_specific_fields, on
// the first frame of a stream or on the message of a plain completion,
// and must come back on the assistant message they belong to — the
// shape recorded from Eden on 2026-10-02.
func TestThoughtSignaturesRoundTrip(t *testing.T) {
	stream := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"provider_specific_fields\":{\"thought_signatures\":[\"sig-a\"]}}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"```javascript\\nlog(1)\\n```\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	complete := `{"choices":[{"message":{"content":"ok","provider_specific_fields":{"thought_signatures":["sig-b"]}},"finish_reason":"stop"}]}`

	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, stream)
			return
		}
		_, _ = io.WriteString(w, complete)
	}))
	defer srv.Close()
	c := testClient(t, srv, -1)

	got, err := c.Stream(context.Background(), CompletionRequest{Messages: []Message{{Role: "user", Content: "hi"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.ThoughtSignatures, []string{"sig-a"}) {
		t.Errorf("stream signatures = %v", got.ThoughtSignatures)
	}

	got, err = c.Complete(context.Background(), CompletionRequest{Messages: []Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "```javascript\nlog(1)\n```", ThoughtSignatures: []string{"sig-a"}},
		{Role: "user", Content: "Execution result: 1"},
		{Role: "assistant", Content: "no signatures here"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.ThoughtSignatures, []string{"sig-b"}) {
		t.Errorf("complete signatures = %v", got.ThoughtSignatures)
	}

	var sent struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(bodies[1], &sent); err != nil {
		t.Fatal(err)
	}
	for i, m := range sent.Messages {
		psf, has := m["provider_specific_fields"]
		switch i {
		case 2:
			if string(psf) != `{"thought_signatures":["sig-a"]}` {
				t.Errorf("the assistant reply should carry its signature back, got %s", psf)
			}
		default:
			// A host that does not know the field must never see it.
			if has {
				t.Errorf("message %d should not carry provider_specific_fields: %s", i, psf)
			}
		}
	}
}
