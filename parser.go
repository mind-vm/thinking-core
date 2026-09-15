package agentloop

import (
	"regexp"
	"strings"
)

// jsFenceRe matches the first ```javascript or ```js fenced block.
// (?s) enables dotall so the body can contain newlines; (?:javascript|js)
// accepts both aliases the model uses.
var jsFenceRe = regexp.MustCompile("(?s)```(?:javascript|js)\\s*\n(.*?)\n```")

// ExtractJSBlock returns the contents of the first ```javascript or
// ```js fenced block, or "" if none is present. Whitespace inside the
// block is trimmed — some models add a blank line right after the fence
// and the intent is unaffected.
func ExtractJSBlock(s string) string {
	m := jsFenceRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// fenceOpenRe matches a line that opens a fenced block: three or more
// backticks and an optional language tag.
var fenceOpenRe = regexp.MustCompile("^(`{3,})([A-Za-z0-9_+-]*)\\s*$")

// ExtractDataBlocks returns the body of every fenced block in s except the
// first ```javascript or ```js one, which is the script. They let a reply
// carry Go source or markdown as raw text beside the script, so file
// contents and answers need no escaping inside a template literal. A
// block closes on a line of exactly its own backticks, so a longer fence
// can hold a shorter one.
func ExtractDataBlocks(s string) []string {
	out := []string{}
	scriptSeen := false
	lines := strings.Split(s, "\n")
	for i := 0; i < len(lines); i++ {
		m := fenceOpenRe.FindStringSubmatch(strings.TrimRight(lines[i], "\r"))
		if m == nil {
			continue
		}
		var body []string
		closed := false
		j := i + 1
		for ; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == m[1] {
				closed = true
				break
			}
			body = append(body, lines[j])
		}
		if !closed {
			break
		}
		i = j
		if !scriptSeen && (m[2] == "javascript" || m[2] == "js") {
			scriptSeen = true
			continue
		}
		out = append(out, strings.Join(body, "\n"))
	}
	return out
}

// doneMarkerRe matches "DONE" on its own line (optionally indented)
// followed by the final answer on subsequent lines.
var doneMarkerRe = regexp.MustCompile(`(?m)^\s*DONE\s*\n([\s\S]*)`)

// ExtractDoneMarker reports whether s contains a terminating DONE
// marker, and if so returns the answer that follows it. The marker must
// be on its own line — an inline "DONE" inside prose doesn't prematurely
// terminate the run. answer() is the documented way to finish; this is
// a defensive fallback for a model that emits the legacy marker instead.
func ExtractDoneMarker(s string) (done bool, final string) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "DONE" {
		return true, ""
	}
	m := doneMarkerRe.FindStringSubmatch(s)
	if m == nil {
		return false, ""
	}
	return true, strings.TrimSpace(m[1])
}
