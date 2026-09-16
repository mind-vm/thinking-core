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

// EnableThink adds think(...), which writes the model's own reasoning into
// the log it reads back. A plan written as comments disappears once the
// loop elides the script that held it; the log of each turn stays in the
// history, so a thought written with think() is still there turns later.
func (s *Sandbox) EnableThink() {
	if s.thinkEnabled {
		return
	}
	s.thinkEnabled = true
	_ = s.runtime.Set("think", func(call goja.FunctionCall) goja.Value {
		s.emitConsole("think", argStrings(call.Arguments)...)
		return goja.Undefined()
	})
	if s.baseKeys != nil {
		s.baseKeys["think"] = struct{}{}
	}
}

// EnableState adds `state`, one object that survives between scripts
// while everything else is reset. It is the narrow form of keepGlobals:
// the model gets somewhere to keep what it learned without the whole
// runtime becoming a REPL.
func (s *Sandbox) EnableState() {
	if s.stateEnabled {
		return
	}
	s.stateEnabled = true
	_ = s.runtime.Set("state", s.runtime.NewObject())
	if s.baseKeys != nil {
		s.baseKeys["state"] = struct{}{}
	}
}

// EnablePlan adds plan(...steps), which records the model's intended
// steps and repeats them at the head of every later script's result.
// think() writes once into one turn's log; a plan is restated until it
// is replaced, so the model is reminded of it rather than having to
// find it in the history.
func (s *Sandbox) EnablePlan() {
	if s.planEnabled {
		return
	}
	s.planEnabled = true
	_ = s.runtime.Set("plan", func(call goja.FunctionCall) goja.Value {
		s.planSteps = argStrings(call.Arguments)
		s.Emit(Event{Kind: EventPlan, Summary: "plan", Detail: strings.Join(s.planSteps, "; ")})
		return goja.Undefined()
	})
	if s.baseKeys != nil {
		s.baseKeys["plan"] = struct{}{}
	}
}

// planHeader is the standing plan, prefixed to a turn's result.
func (s *Sandbox) planHeader() string {
	if len(s.planSteps) == 0 {
		return ""
	}
	return "PLAN (yours, until you call plan() again):\n- " + strings.Join(s.planSteps, "\n- ") + "\n"
}
