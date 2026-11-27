# ADR-0019: A configurable service beside the library — JSON config, secrets by env name

- **Status:** Exploring
- **Confidence:** Medium
- **Decided:** 2026-11-27
- **Last reviewed:** 2026-11-27
- **Consumers:** backends in other languages, through `agentloop serve`
  ([docs/cli/serve.md](../cli/serve.md)); the Go library is unchanged.

## Context

The library serves Go applications, which compose capabilities in code
([ADR-0009](0009-thin-core-optional-packages.md)). A Python or Node backend
cannot do that, and the only door open to it was the CLI's `--json` mode: one
process per run, no way to answer `ask()`, no way to watch a run twice.
Two Go applications had already written the same HTTP/SSE transport
(`agentrun`), so the pieces existed.

## Decision

`agentloop serve --config <file>` is a subcommand of the existing binary,
exposing runs over HTTP and Server-Sent Events, built from `agentrun`,
`agentloopsql` and the packs the CLI already uses.

- **A service beside the library, not instead of it.** The core gains nothing;
  the service is an application of the seams ([ADR-0010](0010-store-interfaces-and-conformance.md)).
- **JSON config.** The standard library reads it, so no dependency is added for
  people who only wanted a different language. Unknown fields are an error.
- **Secrets by env-var name, never by value.** The file can be committed and
  pasted into a bug report. A missing variable fails at startup.
- **Authentication is one bearer token, one tenant**, mandatory unless the
  server listens on loopback.
- **Runs outlive the request that started them**, with a bounded in-memory event
  buffer so a late subscriber replays from the start.

## Consequences

**Buys.** Any language can use the engine with `curl`-level effort, and
capabilities are an operator's choice rather than a recompile.

**Costs.** A second public surface to keep stable (the HTTP API and the config
schema). A run's replay buffer lives in process memory, so a restart loses
in-flight streams; the session store keeps the outcome.

## What would change our mind

- Callers who need more than one tenant per process: replace the single token
  with a table and pass the tenant to the broker, which already compares it.
- A config that wants conditionals or composition: stop growing JSON and make
  the service a Go library users embed.

## Cost of change

Cheap before anyone depends on the field names and event shapes; each released
field is a compatibility promise.

## Revisions

- 2026-11-27 — Decided and recorded.
