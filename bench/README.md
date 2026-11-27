# bench: code-as-action vs tool calling

The library's central claim ([ADR-0004](../docs/adr/0004-code-as-action.md),
[ADR-0003](../docs/adr/0003-a-bounded-work-horse.md)) is that on
retrieve → join → compute → report work, an agent that writes a script and
keeps intermediate data server-side uses fewer tokens and turns, and gets
more answers right, than plain tool calling. Until now that rested on one
measurement (the spend-cap case: 2 turns, ~11k prompt tokens vs ~27k and a
wrong answer), and ADR-0004 says so: "not on a comparison". This package
is that comparison, made repeatable across tasks and models.

It has not been run against a real model yet. No numbers are claimed
here; results go in a later change, together with whatever the ADRs then
need to say.

## What is measured

Two agents answer the same tasks over the same backends, against the same
OpenAI-wire endpoint and model:

| agent | how it acts |
|---|---|
| `thinking-core` | `agentloop.Loop` with its default prompts. The backends are reachable as `require('mcp/data')` through `ext.MCPPack`, the shipped way to give the loop someone else's tools. The model writes JavaScript; rows stay in the sandbox; it sees the shape of `state` and what it `log()`s. |
| `tool-calling` | The plain native function-calling loop: the same tools in `tools`, the model answers with `tool_calls` (parallel calls taken as emitted), each result goes back as JSON in a `tool` message, the run ends on the first reply without tool calls. Tool errors go back to the model so it can retry; transient HTTP errors are retried; nothing is truncated. |

Both get the same one-paragraph persona (`bench.Persona`), the same user
message, the same turn cap (`-max-turns`, 20) and the same per-run
timeout. The tools are defined once (`bench.Tools`): the same name,
one-line description (which states the return shape), JSON Schema and
handler serve both sides.

The backends are an in-memory CRM (400 customers), billing (~3,400
invoices for 2026) and product usage (8,000 events, Jan–Jun 2026),
generated from `-seed` by a specified PRNG: the same seed gives the same
data and answers on every machine. `list_customers` pages at 50,
`list_invoices` at 200, `query_usage` returns every match.

### Tasks

Every task has an exact answer, computed by a Go reference solution from
the fixture data and checked programmatically. No judge model.

| id | shape | what the agent has to do |
|---|---|---|
| `lookup` | single tool call, no join | plan and country of one customer. The control: code-as-action can only add overhead here, and the baseline should tie or win. |
| `overdue_de` | join, filter, sum | count and total of overdue invoices of German customers |
| `pro_signups_2025` | pagination, filter, count | pro-plan customers who signed up in 2025 (no signup filter; three pages) |
| `top_enterprise_h1` | join, group-by, top-N | the 5 enterprise customers with the most paid revenue in H1, over ~1k invoices |
| `manager_march` | pagination ×8, join, argmax | the account manager whose customers paid the most in March |
| `top_feature_march` | large intermediate result | the feature with the most usage units in March (~1,400 events, ~100 KB of JSON) |
| `free_api_q2` | join, filter, top-N | the 3 free-plan customers with the most `api` usage in Q2 |
| `overdue_enterprise_usage` | three-source join | among enterprise customers with an overdue invoice, the one with the most Q1 usage |

`go run ./bench/cmd/bench -list` prints them with their expected answers
for a seed.

### Metrics

One JSONL line per (task, agent, model, repetition):

- `correct` — the final answer's JSON matches the expected value. Values
  are compared exactly (integers in cents, ids, order of a top-N); only
  presentation is forgiven (a code fence or sentence around the JSON,
  extra keys, case and whitespace in strings, a number sent as a
  string). `check_detail` says what was wrong.
- `turns` — LLM round-trips. `tool_calls` — fixture tool invocations.
- `prompt_tokens`, `completion_tokens` — summed from each response's
  `usage`, metered at the client for both agents, so both are counted at
  the same place: what crossed the wire.
- `wall_ms`, `error` (a transport error, the turn cap, a context
  overflow), `answer` (clipped).

`summary.md` has a table per model with one row per agent — accuracy,
median prompt and completion tokens, median turns, tool calls and wall
time, error count — and a per-task breakdown. Medians count every run,
failed ones included: a run that spent 80k tokens on a wrong answer still
spent them.

## How to run

No paid API is called by `go test`; the tests use a scripted endpoint. A
real run:

```sh
export OPENAI_API_KEY=...            # any value for a local server
go run ./bench/cmd/bench \
  -base-url https://openrouter.ai/api/v1 \
  -models openai/gpt-4o-mini,google/gemini-2.5-flash \
  -reps 3 -out bench-results/run1
```

Flags: `-models` (comma-separated, default `$OPENAI_CHAT_MODEL`),
`-base-url` (default `$OPENAI_BASE_URL`, else OpenAI), `-reps` (3),
`-seed` (1), `-tasks` (subset by id), `-agents` (`thinking-core`,
`tool-calling`), `-max-turns` (20), `-timeout` (10m per run),
`-parallel` (1). Results are appended to `results.jsonl` as they
complete, so an interrupted run keeps what it paid for; `summary.md` is
written at the end. Run order interleaves the agents, so a provider that
gets slower or flakier over the run affects both alike.

A run of all 8 tasks costs, per model and repetition, very roughly
50–100k prompt tokens on the thinking-core side and 250–400k on the
baseline side, whose prompts re-send every row fetched so far on every
turn. Try one task with `-reps 1` first.

## How to read the results

- **Accuracy first, then tokens.** A cheaper wrong answer is not a win.
  Compare tokens and turns between agents on the tasks both got right
  (the per-task table).
- **Look at `lookup`.** If thinking-core wins the control, something
  other than the mechanism is being measured (a prompt difference, a
  metering bug).
- **Variance.** Three repetitions show whether a difference is stable;
  they do not make it significant. Re-run with another `-seed` before
  believing a small gap.
- **Errors are results.** A baseline run that overflows the context
  window, or a script loop that hits the turn cap, is a failure of that
  approach on that model, and is counted as one.

## Threats to validity

- **Prompt quality on both sides.** thinking-core uses its tuned default
  prompts; the baseline uses a short persona and the provider's own
  tool-calling training. A better baseline system prompt (or a worse
  thinking-core one) moves the numbers. The persona is shared to keep
  this as small as possible, and both prompts are in the code to read.
- **Model choice.** Providers optimise hard for native tool calling, and
  some small models are weak at writing JavaScript. Results for one model
  say little about another; run several, including the cheap default
  ADR-0003 argues for.
- **Fixture design favours joins.** Seven of eight tasks join, aggregate
  or page over hundreds of rows — the workload the claim is about, chosen
  on purpose, not a sample of all agent work. Tool design matters too:
  `query_usage` is unpaginated and there are no server-side aggregate
  tools; an API that offered `sum_usage_by_feature` would make several
  tasks trivial for the baseline. That is a real-world option the
  comparison does not cover.
- **Mental arithmetic.** The baseline must sum hundreds of numbers in
  the model's head, or ask for a code tool it is not given. That is what
  plain tool calling is, but a baseline with a calculator or code tool
  would be a different, stronger comparison.
- **Token metering.** Counts come from the endpoint's `usage`; a gateway
  that omits or estimates it skews both sides, and cached prompt tokens
  are not separated.
- **Checker strictness.** An answer correct in substance but in the
  wrong JSON shape fails. Both agents are given the same format line, and
  `check_detail` shows when this happens.

## Tests

`go test -race ./bench/...` covers fixture determinism (and pins seed 1,
so a generator change that would make old results incomparable fails
loudly), that the reference answers agree with answering through the
tools, the checker, the summary, and both agents end to end against a
scripted OpenAI-wire endpoint on two tasks — including a model that
misreports its result, to prove a wrong answer is scored wrong. The
scripted replies are computed from what each agent sent back, so a pass
means the data flowed through each loop.
