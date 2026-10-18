# ADR-0017: Scripts keep data in state, not in a run(args) return

- **Status:** Working
- **Confidence:** Medium
- **Decided:** 2026-10-18
- **Last reviewed:** 2026-10-18
- **Consumers:** the library's public API — every prompt the loop composes, and
  `sandbox.Sandbox.ExecuteTurn`.

## Context

Each turn the model had to implement `function run(args)` and return whatever
it wanted to keep. The convention worked, but it had to be taught: about 3.6 KB
of protocol prompt explained run, return, args and why only the return
survives, on every call. A model writing a script without being told reaches
for something else: plain top-level code, and a variable to keep things in.
The savings from ADR-0005 come from showing the carried value's shape instead
of its values, not from `return` as such. thinking-core's first version had a
persistent `data` global; it was dropped because its values were re-serialised
into the prompt, not because a global is the wrong way to write.

## Decision

A reply is a plain script. The carried value is the global `state`: the
sandbox injects it before the script, reads it back afterwards (so both
`state.x = …` and `state = {…}` carry), and the loop shows its shape as
before. A script that throws carries nothing, so state stays as it was. Each
script still runs in its own closure, so locals never leak and a top-level
`const` can be declared again. `run(args)`, `args`, `SetRunLeading` and
`RunRequest.InitialArgs` are gone; `RunRequest.InitialState` seeds state.
The protocol prompt is four lines and names only facts about this sandbox.

## Consequences

**Buys.**
- The model writes the way it already would, so there is nothing to learn
  and nothing to forget (no forgotten field in a return, no re-packing).
- A shorter prompt: the protocol went from 3,652 to 514 bytes, and the default
  system prompt from 8,915 to 4,804. On the documents benchmark the same task
  took ~2.7k estimated input tokens instead of ~7.3k.

**Costs.**
- Every consumer's prompts, examples and tests that wrote `function run(args)`
  must be rewritten.
- The long prompt's guards (no tool-call JSON, don't guess names, combine
  steps) are gone from the default. They were measured on weaker models in
  labs/codeloop; nothing yet shows the short prompt holds there.

## What would change our mind

- An eval on a small or local model where the short prompt fails clearly more
  often than the old contract: put back the guard that fixes it, one at a time.
- Models routinely clobbering `state` by accident (`state = rows` where they
  meant `state.rows = rows`): consider merging instead of replacing.

## Cost of change

Cheap inside the module: the sandbox wrapper and the prompt. Expensive
downstream, because every consumer's scripts and prompts change with it.

## Revisions

- 2026-10-18 — Decided and recorded, replacing the run(args) contract
  described in ADR-0004 and ADR-0005.
