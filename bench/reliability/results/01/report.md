3600 runs, 0 of them infrastructure errors (left out below).

### Pass rate by arm and model

| arm | deepseek-v4.1-flash | glm-5.3-flash | qwen3.6-35b-a3b | gpt-oss-20b | devstral-small | gemini-3.5-flash-lite | all models (95% CI) |
|---|---:|---:|---:|---:|---:|---:|---:|
| A-script | 89% (89/100) | 91% (91/100) | 82% (82/100) | 90% (90/100) | 90% (90/100) | 90% (90/100) | **89% (532/600)** (86–91) |
| B-main | 100% (100/100) | 100% (100/100) | 91% (91/100) | 89% (89/100) | 100% (100/100) | 91% (91/100) | **95% (571/600)** (93–97) |
| C-state | 100% (100/100) | 100% (100/100) | 85% (85/100) | 94% (94/100) | 100% (100/100) | 90% (90/100) | **95% (569/600)** (93–96) |
| D-data | 99% (99/100) | 100% (100/100) | 87% (87/100) | 94% (94/100) | 100% (100/100) | 92% (92/100) | **95% (572/600)** (93–97) |
| E-tools | 81% (81/100) | 90% (90/100) | 70% (70/100) | 89% (89/100) | 80% (80/100) | 80% (80/100) | **82% (490/600)** (78–85) |
| L-loop | 99% (99/100) | 100% (100/100) | 94% (94/100) | 92% (92/100) | 100% (100/100) | 90% (90/100) | **96% (575/600)** (94–97) |

### Pass rate by task and arm (all models)

| task | probes | A-script | B-main | C-state | D-data | E-tools | L-loop |
|---|---|---:|---:|---:|---:|---:|---:|
| 01-lookup | one call, then finish | 100% (60/60) | 98% (59/60) | 100% (60/60) | 100% (60/60) | 98% (59/60) | 100% (60/60) |
| 02-chain | a call that needs the previous call's result | 100% (60/60) | 100% (60/60) | 100% (60/60) | 100% (60/60) | 87% (52/60) | 100% (60/60) |
| 03-sum | computing over a list instead of guessing | 87% (52/60) | 92% (55/60) | 88% (53/60) | 87% (52/60) | 53% (32/60) | 90% (54/60) |
| 04-carry | data kept across turns, read once | 2% (1/60) | 70% (42/60) | 62% (37/60) | 68% (41/60) | 5% (3/60) | 70% (42/60) |
| 05-recover | a failing call retried, the run goes on | 100% (60/60) | 98% (59/60) | 100% (60/60) | 100% (60/60) | 78% (47/60) | 100% (60/60) |
| 06-args | an object argument with an enum and an array | 100% (60/60) | 100% (60/60) | 100% (60/60) | 100% (60/60) | 98% (59/60) | 100% (60/60) |
| 07-followup | a second question in the same session | 98% (59/60) | 95% (57/60) | 100% (60/60) | 98% (59/60) | 97% (58/60) | 100% (60/60) |
| 08-notool | answering without touching the API | 100% (60/60) | 100% (60/60) | 100% (60/60) | 100% (60/60) | 100% (60/60) | 100% (60/60) |
| 09-select | the right one of 20 functions | 100% (60/60) | 100% (60/60) | 98% (59/60) | 100% (60/60) | 100% (60/60) | 100% (60/60) |
| 10-once | an effect done exactly once, then stop | 100% (60/60) | 98% (59/60) | 100% (60/60) | 100% (60/60) | 100% (60/60) | 98% (59/60) |

### Paired against C-state (same model, task and repetition)

Discordant pairs only: *C only* is pairs C passed and the other arm failed. p is McNemar's exact two-sided test.

| arm | pairs | C only | arm only | p |
|---|---:|---:|---:|---:|
| A-script | 600 | 39 | 2 | <0.001 |
| B-main | 600 | 11 | 13 | 0.839 |
| D-data | 600 | 8 | 11 | 0.648 |
| E-tools | 600 | 87 | 8 | <0.001 |
| L-loop | 600 | 8 | 14 | 0.286 |

### Why runs failed

| failure | A-script | B-main | C-state | D-data | E-tools | L-loop |
|---|---:|---:|---:|---:|---:|---:|
| bad_args | 0 | 0 | 0 | 0 | 1 | 0 |
| lost_data | 34 | 6 | 4 | 0 | 3 | 3 |
| no_answer | 0 | 1 | 0 | 0 | 0 | 0 |
| timeout | 4 | 0 | 0 | 0 | 2 | 0 |
| turn_cap | 3 | 2 | 0 | 1 | 6 | 10 |
| wrong_answer | 27 | 20 | 27 | 27 | 98 | 12 |
| *infrastructure* | 0 | 0 | 0 | 0 | 0 | 0 |

### Cost and effort per run (means)

| arm | turns | first reply valid (without 05) | script/tool errors | prompt tok | completion tok | $ per run | $ per pass |
|---|---:|---:|---:|---:|---:|---:|---:|
| A-script | 1.6 | 99% (534/540) | 0.19 | 1362 | 464 | 0.00057 | 0.00064 |
| B-main | 1.8 | 99% (536/540) | 0.14 | 1352 | 325 | 0.00048 | 0.00051 |
| C-state | 1.4 | 99% (537/540) | 0.13 | 937 | 192 | 0.00032 | 0.00033 |
| D-data | 1.5 | 100% (539/540) | 0.11 | 1850 | 178 | 0.00048 | 0.00051 |
| E-tools | 2.8 | 100% (539/540) | 0.15 | 4160 | 758 | 0.00100 | 0.00123 |
| L-loop | 2.2 | 99% (534/540) | 0.17 | 1547 | 258 | 0.00050 | 0.00052 |
