package agentloop

import "encoding/json"

// assistantArgs is the ToolArgs an assistant step (execute_js, response)
// is persisted with: "{}", or the reply's thought signatures. Storing
// them in ToolArgs needs nothing from a store but what the StepStore
// contract already requires — that ToolArgs survives a round trip — so
// a session rehydrated in a later Run still sends them back.
func assistantArgs(sigs []string) json.RawMessage {
	if len(sigs) == 0 {
		return json.RawMessage(`{}`)
	}
	b, err := json.Marshal(struct {
		ThoughtSignatures []string `json:"thought_signatures"`
	}{sigs})
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

// signaturesFrom reads assistantArgs back. Anything else — "{}", an
// older row, a checkpoint marker — yields none.
func signaturesFrom(args json.RawMessage) []string {
	if len(args) == 0 {
		return nil
	}
	var v struct {
		ThoughtSignatures []string `json:"thought_signatures"`
	}
	if json.Unmarshal(args, &v) != nil {
		return nil
	}
	return v.ThoughtSignatures
}
