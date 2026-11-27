# bench/reliability — step 1: how reliable is each way of acting?

thinking-core lets a model act by writing JavaScript, and carries data
between scripts in a global `state` ([ADR-0004](../../docs/adr/0004-code-as-action.md),
[ADR-0017](../../docs/adr/0017-scripts-keep-data-in-state.md)). Both records say
the choice was never measured against the alternatives. This package
measures the mechanics on ten small tasks, each probing one property, so that
only the mechanics can fail; it compares six ways of acting, not two.
[The bench one level up](../README.md) is the larger test: retrieve → join →
compute over a CRM, billing and usage, thinking-core against tool calling.

## The arms

Every arm gets the same functions, generated from one description
(`world.go`): as TypeScript `declare` lines for the code arms, as
JSON-schema tools for E. The bench drives A–E itself, with the same client,
retries, turn cap (8 per message), timeouts and messages; only how data
reaches the next step differs.

| arm | how the model acts | what carries to the next step |
|---|---|---|
| A-script | one JavaScript block per reply | nothing; only what it logged is in the conversation |
| B-main | a block defining `function main(state) { … return state }` | what `main` returned; its shape is shown |
| C-state | a plain script (the library's protocol today) | the global `state`; its shape is shown |
| D-data | a plain script over a global `data` | `data`; its *values* are shown each turn (the first thinking-core) |
| E-tools | native tool calls | every tool result, in the conversation |
| L-loop | C through the real `agentloop` | as C, plus the library's guards (error hints, elision, held answers) |

## The tasks

| id | probes |
|---|---|
| 01-lookup | one call, then finish |
| 02-chain | a call that needs the previous call's result |
| 03-sum | computing over a list instead of guessing |
| 04-carry | data kept across turns: a metered history readable once, then a question about it |
| 05-recover | a call that fails once with a retryable error |
| 06-args | an object argument with an enum and an array |
| 07-followup | a second question in the same session |
| 08-notool | answering without touching the API |
| 09-select | the right one of 20 functions |
| 10-once | an effect done exactly once, then stop |

Expected answers are computed from the same data the agent reads; the checks
are code, not a judge model. A failed run gets one failure class
(`classify` in `arms.go`): turn_cap, timeout, unknown_function,
lost_variable, lost_data, unneeded_calls, duplicate_effect, bad_args,
no_answer, wrong_answer. Gateway errors count as infrastructure and are left
out of the rates.

## Running it

```sh
export EDENAI_API_KEY=... EDENAI_BASE_URL=https://api.eu.edenai.run/v3/
go run ./bench/cmd/reliability run -reps 10 -budget 5 -out bench/reliability/results/01/results.jsonl
go run ./bench/cmd/reliability report -in bench/reliability/results/01/results.jsonl
```

`run` resumes: runs already in the results file are skipped, so a run that
was interrupted or hit the budget continues where it stopped. Transcripts land
next to the results (`transcripts/`, not committed). `go test ./...` drives
every arm end to end against a scripted fake model, with no key; the root
`go test ./...` runs it.

The models are in `../cmd/reliability/main.go`: open-weight or cheap models on
Eden AI's EU endpoint with zero data retention, at their list price.

Results: [docs/bench/01-reliability.md](../../docs/bench/01-reliability.md).
