package agentloop

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// sanitizeErrorForModel strips runtime plumbing from a sandbox error so
// the model sees only the semantic message. It removes goja's appended
// stack frames (which leak internal Go symbols like
// "FetchPack.func3 (native)" and mean nothing to the agent) and the
// "sandbox:"/"GoError:" wrapper prefixes, while preserving JS error
// kinds (ReferenceError/TypeError) that retryHintForError and the model
// rely on.
//
//	raw:  "sandbox: GoError: fetch: dial tcp: i/o timeout at agentloop/sandbox.doFetchRequest.func1 (native)"
//	out:  "fetch: dial tcp: i/o timeout"
func sanitizeErrorForModel(raw string) string {
	msg := raw

	// goja puts the message on the first line and stack frames after it; drop them.
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	// When newlines were flattened, frames trail inline as " at <sym> (native)"
	// or " at <fn> (<eval>:…)" / " at <eval>:l:c(pc)". Cut from the first such
	// frame marker; a plain " at " with neither marker is left as real content.
	if i := strings.Index(msg, " at "); i >= 0 {
		tail := msg[i:]
		if strings.Contains(tail, "(native)") || strings.Contains(tail, "<eval>") {
			msg = msg[:i]
		}
	}

	msg = strings.TrimSpace(msg)
	msg = strings.TrimPrefix(msg, "sandbox: ")
	// GoError: is goja's wrapper for a thrown Go error — noise. (ReferenceError/
	// TypeError are real JS kinds and are deliberately left intact.)
	msg = strings.ReplaceAll(msg, "GoError: ", "")
	return strings.TrimSpace(msg)
}

// retryHintForError pattern-matches common script failure modes and
// returns a concrete instruction string appended to the error fed back
// to the model. Empty string when there's no specific hint — the model
// then reasons from the raw error alone. Intentionally generic (no
// application-schema knowledge): an application with its own capability
// packs and error shapes (foreign-key violations, enum constraints,
// domain-specific field names) should layer its own hints in front of
// this — sort by specificity and return the first non-empty result.
func retryHintForError(errMsg string) string {
	lower := strings.ToLower(errMsg)

	// Transient infrastructure errors — timeouts and cancellations. These are
	// NOT the model's fault, and a write may or may not have landed, so
	// blindly repeating it risks a duplicate. Tell it to verify first.
	if strings.Contains(lower, "context deadline exceeded") ||
		strings.Contains(lower, "context canceled") ||
		strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out") {
		return "This was a transient timeout, not a problem with your code. The operation may or may not " +
			"have completed. Before retrying a write, read the current state back first to check whether it " +
			"already succeeded — retry only if it did not. Do not repeat it blindly."
	}

	// ReferenceError / hallucinated APIs.
	if strings.Contains(lower, "referenceerror") {
		return "You referenced a function or variable that doesn't exist. All sandbox functions are " +
			"GLOBAL — no namespace prefix, no this.x — and the declarations under Sandbox API in the " +
			"system prompt are the complete list."
	}

	// TypeError on result properties — usually a shape mismatch.
	if strings.Contains(lower, "typeerror") && strings.Contains(lower, "of undefined") {
		return "You tried to read a property of an undefined value. Most likely the call's return shape " +
			"isn't what you assumed — check its declaration under Sandbox API in the system prompt."
	}

	return ""
}

// scriptLineOffset is how many lines the sandbox's wrapper puts above the
// model's script, so an engine line number minus this is the script's.
const scriptLineOffset = 1

var (
	syntaxLocRe = regexp.MustCompile(`Line (\d+):(\d+)`)
	evalLocRe   = regexp.MustCompile(`<eval>:(\d+):(\d+)`)
)

// scriptLocation finds where an engine error points in the model's own
// script: its 1-based line and column, and whether it lies past the end.
func scriptLocation(raw, code string, preludeLines int) (line, col int, pastEnd, ok bool) {
	m := syntaxLocRe.FindStringSubmatch(raw)
	if m == nil {
		m = evalLocRe.FindStringSubmatch(raw)
	}
	if m == nil {
		return 0, 0, false, false
	}
	engineLine, _ := strconv.Atoi(m[1])
	col, _ = strconv.Atoi(m[2])
	line = engineLine - scriptLineOffset - preludeLines
	lines := strings.Split(code, "\n")
	if line < 1 {
		return 0, 0, false, false
	}
	return line, col, line > len(lines), true
}

// describeScriptError is the error a model sees for a failed script: the
// message with the engine's line number replaced by the script's, the
// offending line with a caret under the column, and a hint for the two
// syntax failures the traces show most — a backtick that closes a
// template literal early, and a script that ends inside an open literal.
func describeScriptError(raw, code string, preludeLines int) (message, location, hint string) {
	message = sanitizeErrorForModel(raw)
	line, col, pastEnd, ok := scriptLocation(raw, code, preludeLines)
	if !ok {
		if preludeLines > 0 && (syntaxLocRe.MatchString(raw) || evalLocRe.MatchString(raw)) {
			return message, "while writing the fenced file blocks", ""
		}
		return message, "", ""
	}
	message = syntaxLocRe.ReplaceAllString(message, fmt.Sprintf("Line %d:%d", line, col))
	isSyntax := strings.Contains(message, "SyntaxError")

	lines := strings.Split(code, "\n")
	if pastEnd {
		location = "at the end of your script"
		if isSyntax {
			hint = "The script ended while a template literal, string, bracket or brace was still open. " +
				"A backtick inside a template literal closes it early: escape it as \\`, or put the text " +
				"(Go code, markdown) in its own fenced block after the script and read it as blocks[0]."
		}
		return message, location, hint
	}
	src := lines[line-1]
	caret := ""
	if col > 0 && col <= len(src)+1 {
		caret = "\n  " + strings.Repeat(" ", col-1) + "^"
	}
	location = fmt.Sprintf("at line %d of your script:\n  %s%s", line, src, caret)
	if isSyntax && strings.Count(src, "`") > 0 {
		hint = "A backtick inside a template literal closes it early. Escape it as \\`, or put the text " +
			"(Go code, markdown) in its own fenced block after the script and read it as blocks[0]."
	}
	return message, location, hint
}
