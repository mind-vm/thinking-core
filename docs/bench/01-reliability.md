# Step 1 — how reliable is each way of acting?

Measured 2026-11-27 with [`bench/reliability`](../../bench/reliability/README.md): 3,600 runs, six
arms × six models × ten tasks × ten repetitions, $2.01 in all. Raw results:
[`bench/reliability/results/01/results.jsonl`](../../bench/reliability/results/01/results.jsonl).

## Verdict

**On small, clean tasks, letting the model write code beats native tool
calling clearly, and how data carries between scripts barely matters — as long
as something carries. The library's current design (C, `state`) is as reliable
as any other code arm and the cheapest.**

- **Code over tool calls.** The code arms pass 95–96%, native tool calls 82%
  (C against E: 87 pairs only C passed, 8 only E, p < 0.001). Tool calling
  costs 3.7× as much per passed task ($0.00123 against $0.00033) and twice the
  turns.
- **Something must carry.** A plain script with nothing carried (A) drops to
  89% (39 against 2, p < 0.001), all of it on the one task that needs data
  kept across turns: 2% there.
- **How it carries does not matter for reliability.** `main(state)` (B),
  `state` (C) and a `data` object shown with its values (D) are within noise
  of each other (p = 0.84 and 0.65). The differences are cost: D's prompt
  grows with the data (1.9k tokens a run against C's 0.9k; 10.5k against 1.4k
  on the 240-order task), B writes longer replies.
- **The library's guards help a little, not significantly.** C through the
  real agentloop (L) passes 96% against the bare driver's 95% (14 against 8,
  p = 0.29). Where they help is exactly where they were built for: an
  `answer()` written before reading the output it depends on.
- **The model matters as much as the arm.** Qwen3.6-35B-A3B is the weakest
  under every arm; DeepSeek V4.1 Flash is 100% with code and 81% with tools.

So step 1 confirms [ADR-0017](../adr/0017-scripts-keep-data-in-state.md) on six
cheap models, and gives [ADR-0004](../adr/0004-code-as-action.md) the
head-to-head comparison it said was missing — on easy tasks. Step 2 (SaaS,
large data, a complex API) is where the size of the gap should show.

## Setup

| | |
|---|---|
| Arms | A-script, B-main, C-state, D-data, E-tools, L-loop ([the arms](../../bench/reliability/README.md#the-arms)) |
| Models | DeepSeek V4.1 Flash, GLM-5.3 Flash, Qwen3.6-35B-A3B, gpt-oss-20b, Devstral Small, Gemini 3.5 Flash-Lite — Eden AI EU endpoint, zero data retention |
| Tasks | ten, each probing one property ([the tasks](../../bench/reliability/README.md#the-tasks)) |
| Limits | 8 model calls per message, 4 minutes per run, 4,096 completion tokens per call |
| Checks | code against answers computed from the same data; gateway errors and hung hosts (no reply in 4 minutes) repeated, 45 runs |
| Library | agentloop and sandbox at `8b542be`; main has since bounded carried state (4 MB, far above anything here) and honours context cancellation in scripts |

gpt-oss-120b and MiniMax were planned but are not offered with zero data
retention in the EU; gpt-oss-20b and Devstral Small took their places.

## Pass rate by arm and model

| arm | deepseek-v4.1-flash | glm-5.3-flash | qwen3.6-35b-a3b | gpt-oss-20b | devstral-small | gemini-3.5-flash-lite | all models (95% CI) |
|---|---:|---:|---:|---:|---:|---:|---:|
| A-script | 89% | 91% | 82% | 90% | 90% | 90% | **89%** (86–91) |
| B-main | 100% | 100% | 91% | 89% | 100% | 91% | **95%** (93–97) |
| C-state | 100% | 100% | 85% | 94% | 100% | 90% | **95%** (93–96) |
| D-data | 99% | 100% | 87% | 94% | 100% | 92% | **95%** (93–97) |
| E-tools | 81% | 90% | 70% | 89% | 80% | 80% | **82%** (78–85) |
| L-loop | 99% | 100% | 94% | 92% | 100% | 90% | **96%** (94–97) |

100 runs per cell, 600 per arm.

## Pass rate by task

| task | probes | A | B | C | D | E | L |
|---|---|---:|---:|---:|---:|---:|---:|
| 01-lookup | one call, then finish | 100% | 98% | 100% | 100% | 98% | 100% |
| 02-chain | a call that needs the previous call's result | 100% | 100% | 100% | 100% | 87% | 100% |
| 03-sum | computing over a list instead of guessing | 87% | 92% | 88% | 87% | **53%** | 90% |
| 04-carry | data kept across turns, read once | **2%** | 70% | 62% | 68% | **5%** | 70% |
| 05-recover | a failing call retried, the run goes on | 100% | 98% | 100% | 100% | 78% | 100% |
| 06-args | an object argument with an enum and an array | 100% | 100% | 100% | 100% | 98% | 100% |
| 07-followup | a second question in the same session | 98% | 95% | 100% | 98% | 97% | 100% |
| 08-notool | answering without touching the API | 100% | 100% | 100% | 100% | 100% | 100% |
| 09-select | the right one of 20 functions | 100% | 100% | 98% | 100% | 100% | 100% |
| 10-once | an effect done exactly once, then stop | 100% | 98% | 100% | 100% | 100% | 98% |

60 runs per cell. Six tasks are at or near 100% for every arm: one call,
chaining, argument shapes, staying out of the API, picking one of twenty
functions, doing an effect once. The mechanics themselves are reliable on all
six models; the differences come from three tasks.

## Paired against C-state

Same model, task and repetition; only discordant pairs count. p is McNemar's
exact two-sided test.

| arm | C passed, arm failed | arm passed, C failed | p |
|---|---:|---:|---:|
| A-script | 39 | 2 | < 0.001 |
| B-main | 11 | 13 | 0.84 |
| D-data | 8 | 11 | 0.65 |
| E-tools | 87 | 8 | < 0.001 |
| L-loop | 8 | 14 | 0.29 |

## Why it is so — what failed, and how

**Tool calling loses where arithmetic happens in the model's head.** On
03-sum (13 amounts in cents to add) E passes 53%; DeepSeek, Devstral and
Gemini fail it 8–10 times out of 10. 18 of the 28 failures write the addition
out in the answer and get it wrong (`… + 48920 + 53550 = 804,710 cents`; the
right total is 824,710). Every code arm adds with `reduce` and is right
whenever it reads the right orders. On 04-carry (240 orders, count and sum
the paid ones of one month) E passes 5%: the model counts 21 where there are
23. A tool result is something to read; a value in a script is something to
compute with.

**Tool calling also lets a model skip the tools.** Qwen3.6 under E answered
19 tasks that need data without a single call — "0" open orders, "150" in
stock — which it never does with code. Under code the same model's typical
mistake is a unit: it answers 03-sum with `824710` (cents) instead of
8,247.10 €.

**Without a carry, data that cannot be fetched again is lost.** A passes
04-carry once in 60. 34 of its runs tried `loadOrderHistory()` again and hit
the quota; the rest answered from what they had logged, which the log cap and
the guidance against logging whole datasets keep small. On every task that
needs no carry, A is as good as C.

**The code arms' own failure on 04-carry is answering too early.** The task
needs two looks: load the history, then read the question from
`getQuestion()`. 18 of C's 23 failures, and 18 of D's 19, called `answer()` in
the first script — with a summary of the data, or with the question text
itself — before the model had read what it was asked. The agentloop holds an
`answer()` from a script that also logged output the model has not seen
(`unreadAnswerNotice`); under L only 4 of 18 failures answered in the first
turn. The guard is the reason L leads C, and it is why L's failures on this
task move to the turn cap instead (10 runs, 9 of them Gemini).

**`main(state)` and `data` work, but cost.** B's lost data (6 runs on
04-carry, all Gemini, against C's 4) is not a forgotten `return`: every one
of those `main`s returns `state`, but loads the history into a local `const`
and never puts it there. It is the same slip C makes, with one more place to
make it. D never lost data —
its values are in front of the model every turn — but that is also its
price: 10.5k prompt tokens a run on the 240-order task against C's 1.4k, and
the gap grows with the data. E pays twice that (22.5k), because every tool
result stays in the conversation.

## Cost and effort per run

| arm | turns | first reply valid | prompt tok | completion tok | $ per run | $ per pass |
|---|---:|---:|---:|---:|---:|---:|
| A-script | 1.6 | 99% | 1,362 | 464 | 0.00057 | 0.00064 |
| B-main | 1.8 | 99% | 1,352 | 325 | 0.00048 | 0.00051 |
| C-state | 1.4 | 99% | 937 | 192 | 0.00032 | **0.00033** |
| D-data | 1.5 | 100% | 1,850 | 178 | 0.00048 | 0.00051 |
| E-tools | 2.8 | 100% | 4,160 | 758 | 0.00100 | 0.00123 |
| L-loop | 2.2 | 99% | 1,547 | 258 | 0.00050 | 0.00052 |

"First reply valid" leaves out 05-recover, whose first call fails on purpose.
Every arm's first reply parses and runs (or calls a tool) 99–100% of the
time: none of these models has trouble with the protocol itself.

## What this does not show

- **Easy tasks.** Ten small tasks with five functions; the hard questions —
  large data, paging, a complex API — are step 2's.
- **One prompt per arm.** Each arm has the shortest prompt that states its
  rules; a tuned tool-calling prompt (e.g. "never compute in your head")
  might close part of the arithmetic gap. The code arms got no tuning either.
- **Task 04 measures two things.** Keeping data and reading before answering;
  the failure classes separate them, the pass rate does not.
- **One gateway, one region.** Hosts were fixed per model; one host (Vertex's
  DeepSeek) hung for a stretch, and those runs were repeated.
- **Arm L differs in more than guards:** the library streams, elides old
  scripts and retries differently. Its lead over C is the sum of all that.

## Next

Step 2 is the larger test: [`bench/`](../../bench/README.md), retrieve →
join → compute over an in-memory CRM, billing and usage with paging, the
library against plain tool calling — where the token gap seen on 04-carry
should become a pass-rate gap.
