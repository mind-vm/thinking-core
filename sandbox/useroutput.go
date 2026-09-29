package sandbox

import (
	"strings"

	"github.com/dop251/goja"
)

// EnableUserOutput separates what a script shows the user from what it
// shows the model. print(...) appends to the task's output instead of the
// log, and done(message?) and exit(message?) mark the task finished once
// the script returns. Printed output accumulates across scripts; the done
// flag resets with every ExecuteTurn. Calling it again is harmless.
func (s *Sandbox) EnableUserOutput() {
	if s.userOutput {
		return
	}
	s.userOutput = true
	rt := s.runtime
	_ = rt.Set("print", func(call goja.FunctionCall) goja.Value {
		text := strings.Join(argStrings(call.Arguments), " ")
		s.printed = append(s.printed, text)
		s.logs = append(s.logs, LogEntry{Message: "(printed for the user)"})
		s.Emit(Event{Kind: EventLog, Summary: "print", Detail: text})
		return goja.Undefined()
	})
	finish := func(call goja.FunctionCall) goja.Value {
		s.doneCalled = true
		if len(call.Arguments) > 0 {
			s.doneMessage = strings.Join(argStrings(call.Arguments), " ")
		}
		return goja.Undefined()
	}
	_ = rt.Set("done", finish)
	_ = rt.Set("exit", finish)
	if s.baseKeys != nil {
		s.baseKeys["done"] = struct{}{}
		s.baseKeys["exit"] = struct{}{}
	}
}

// TakeDone reports whether the last script called done() or exit(), and
// the task's output: everything printed so far, then the done message.
func (s *Sandbox) TakeDone() (output string, called bool) {
	if !s.doneCalled {
		return "", false
	}
	parts := append([]string{}, s.printed...)
	if s.doneMessage != "" {
		parts = append(parts, s.doneMessage)
	}
	return strings.Join(parts, "\n"), true
}
