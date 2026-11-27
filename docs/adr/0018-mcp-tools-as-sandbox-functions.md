# ADR-0018: MCP tools as sandbox functions behind an interface; the SDK in its own module

- **Status:** Exploring
- **Confidence:** Medium
- **Decided:** 2026-11-27
- **Last reviewed:** 2026-11-27
- **Consumers:** the library's public API (`ext.MCPPack`), and the `agentloop`
  CLI's `--mcp` flag. No application outside this repository uses it yet.

## Context

MCP servers are where a growing share of tools live, and the usual way to
offer them is one model tool per MCP tool: every call is a round-trip, and every
intermediate result passes through the context window. [ADR-0004](0004-code-as-action.md)
already chose scripts as the unit of action, so the same servers can be
functions inside the sandbox, composed in one script. Speaking MCP needs the
official Go SDK, a large dependency tree, and [ADR-0009](0009-thin-core-optional-packages.md)
keeps those off a deployment that never connects to a server.

## Decision

`ext.MCPPack` takes an `ext.MCPSession` — `ListTools` and `CallTool`, two
methods over plain Go types — and exposes each tool as a synchronous function
of `require('mcp/<server>')`. Tools are listed once, at build time. Every call
is checked against the policy as `mcp.<server>.<tool>`: a tool whose server
says `readOnlyHint` is allowed, any other is a side effect that
`DefaultPolicy` denies unless granted (`mcp.<server>.*` grants a server), and a
denial becomes a question for the user, remembered for the session, as in
`WorkspacePack`.

The implementation over the SDK is `github.com/mind-vm/thinking-core/ext/mcp`,
a separate module, as `browser/chrome` is. Because the CLI is in the root
module and cannot import it, the CLI's body moved to the importable package
`cmd/agentloop/cli`, which takes a `Hooks` value; `cmd/agentloop` passes
none, and `ext/mcp/cmd/agentloop` passes the one that opens MCP servers. The
stock binary rejects `--mcp` with a message saying which build to install.

## Consequences

**Buys.** The root `go.mod` has no MCP dependency, and the pack is tested
against a fake session with no server. A script reads from several tools and
keeps only what it needs. The permission gate is the one every other side
effect already goes through.

**Costs.** `readOnlyHint` is the server's own claim, so a hostile server can
skip the gate; `MCPConfig.IgnoreReadOnlyHint` closes that and costs a prompt
per tool. Two binaries now exist, and only the second speaks MCP. A tool list
fixed at build time misses a server that changes it mid-session. Only text
and structured content reach a script: images and audio become a one-line
placeholder.

## What would change our mind

- If the SDK's dependency tree shrinks to what `ext` already carries, fold the
  client into `ext` and drop the second binary.
- If a second consumer needs servers that announce tool-list changes, make
  listing lazy rather than build-time.
- If callers keep granting `mcp.<server>.*` just to stop the prompts, the
  per-tool approval is the wrong grain.

## Cost of change

Dropping the module is deleting a directory; the interface is two methods, so
a different client can replace it without touching the pack. The `cli`
package split is exported API in a new path: moving it back would break any
importer, of which there is one inside this repository.

## Revisions

- 2026-11-27 — Written.
