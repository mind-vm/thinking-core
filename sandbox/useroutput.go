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
		if s.exitGuard != nil {
			if err := s.exitGuard(); err != nil {
				panic(rt.NewGoError(err))
			}
		}
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

// SetExitGuard installs a check done() and exit() consult before they
// mark the task finished; an error refuses the call and reaches the
// model as the exception. Nil removes it.
//
// It exists for a finish that should wait: a script that edits a file,
// runs a check and calls exit() in one breath ends the task on output
// the model never read. TurnEffects is what a guard usually asks.
func (s *Sandbox) SetExitGuard(fn func() error) { s.exitGuard = fn }

// TurnEffects lists what the current script has changed or run so far —
// the summaries of its file_write and exec events — in order. It resets
// with every Execute and ExecuteTurn.
func (s *Sandbox) TurnEffects() []string { return append([]string(nil), s.turnEffects...) }
