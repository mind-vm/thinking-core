---
description: Run agentloop as an HTTP service — start runs with a POST, watch them as Server-Sent Events, configured by a JSON file instead of Go code.
sidebar:
  order: 2
---

# `agentloop serve`

`agentloop serve --config <file>` exposes the engine over HTTP so a backend in
any language can use it: POST a task, read the event stream. The capabilities
the agent has come from the config file, not from Go code.

```sh
export OPENAI_API_KEY=sk-...           # the same LLM settings as the CLI
export THINKING_CORE_TOKEN=$(openssl rand -hex 24)
agentloop serve --config thinking-core.json
```

LLM settings are the CLI's: `OPENAI_API_KEY`, `OPENAI_BASE_URL`,
`OPENAI_CHAT_MODEL`, and the flags `--model`, `--timeout`, `--history-window`,
`--context-window`, `--quiet`. `SIGINT`/`SIGTERM` stop accepting work, cancel
the running runs (each still ends its stream with `done`), and close the store.

## Config

JSON, and unknown fields are an error, so a typo fails at startup instead of
quietly turning a restriction off. **Secrets are never in the file**: each is
named by the environment variable that holds it. Relative paths resolve
against the config file's directory.

```json
{
  "listen": "127.0.0.1:8080",
  "store": "thinking-core.db",
  "tokenEnv": "THINKING_CORE_TOKEN",
  "maxIterations": 20,
  "policy": {
    "allowTools": [],
    "urlAllowPrefixes": ["https://crm.internal/"],
    "allowCommands": [],
    "allowPrivateNetworks": true
  },
  "openapi": [
    {
      "name": "crm",
      "spec": "./crm.yaml",
      "baseURL": "https://crm.internal/api",
      "headersEnv": { "Authorization": "CRM_AUTH" }
    }
  ],
  "workspace": null,
  "capabilities": ["fetch", "ai", "markdown"]
}
```

| Field | Meaning |
| --- | --- |
| `listen` | `host:port`. Default `127.0.0.1:8080`. |
| `store` | SQLite file for sessions and steps (`agentloopsql`). Default `thinking-core.db`. |
| `tokenEnv` | Env var holding the bearer token. Default `THINKING_CORE_TOKEN`. |
| `maxIterations` | LLM round-trips per run; 0 is the engine default (same as `--max-steps`). |
| `policy` | The fields of `sandbox.DefaultPolicy`: `allowTools`, `urlAllowPrefixes`, `allowCommands`, `allowPrivateNetworks`. |
| `openapi[]` | One skill per entry: `spec` (OpenAPI 3 file), optional `name`, `description`, `baseURL`, `methods`, and `headersEnv` (header name to env var name). The agent calls it with `require("crm")`. |
| `workspace` | `null` (no filesystem) or `{"root": "dir"}` for `readFile`/`writeFile`/`grep`… confined to that directory. |
| `capabilities` | Built-ins to enable: `markdown`, `http`, `fetch`, `ai`, `ask`, `exec`. Omitted means `markdown, http, fetch, ai, ask`. `exec` needs a `workspace` and is never a default. |

An `"mcp": [...]` section will join these once MCP support lands.

**Auth.** Every `/v1/` request needs `Authorization: Bearer <token>`. If the
env var is unset the server only starts when `listen` is a loopback address
(and logs a warning); on any other address it refuses to start. One token is
one tenant.

**Questions the agent asks.** `ask()`, and the approval a fetch to a new
domain or a workspace write would need on a terminal, become `input_request`
events. Answer them over the API, or narrow `policy` so they are never raised.

## API

| Request | Result |
| --- | --- |
| `POST /v1/runs` `{"message", "sessionId"?, "context"?}` | `202 {"runId","sessionId"}`. The run proceeds in the background. `sessionId` must be a UUID; omit it to start a session, send it back to continue one. A session runs one thing at a time (`409` otherwise). |
| `POST /v1/runs?wait=true` | Blocks, then `200` with the same body as `GET /v1/runs/{id}`. A failed run is still `200` with `"status":"error"`. |
| `GET /v1/runs/{id}/events` | Server-Sent Events: every event from the start of the run, then live, ending with exactly one `done`. |
| `GET /v1/runs/{id}` | `{"runId","sessionId","status","answer","error","summary"}`; `status` is `running`, `completed`, `max_iterations`, `canceled` or `error`. |
| `POST /v1/runs/{id}/answer` `{"requestId","value"}` | `204`; `409` when nothing is waiting on that id. |
| `GET /v1/sessions?limit=50` | `{"sessions":[{id,status,steps,tokens,createdAt,updatedAt,opening}]}` |
| `DELETE /v1/sessions/{id}` | `204`, `404`, or `409` while a run is using it. |
| `GET /healthz` | `200`, no token needed. |

Event names are the loop's: `user`, `execute_js`, `execute_js_result`,
`response_chunk`, `response`, `input_request`, `warning`, `error`, `done`. The
`input_request` event carries `args.requestId`, `args.inputType`, and
`content` (the question).

**Replay and its bound.** Each run keeps its events in memory, so a client that
connects late, or twice, sees the run from the start. A run keeps at most
10,000 events (past that only `error`/`done` are kept, after a `warning`), and
a finished run is forgotten an hour after it ends — its answer stays in the
session store. A client that disconnects does not stop its run, including
`wait=true`.

### curl

```sh
T="Authorization: Bearer $THINKING_CORE_TOKEN"
curl -s -H "$T" -d '{"message":"What is 12 * 7?"}' localhost:8080/v1/runs
# {"runId":"…","sessionId":"…"}
curl -sN -H "$T" localhost:8080/v1/runs/<runId>/events
curl -s  -H "$T" -d '{"message":"What is 12 * 7?"}' 'localhost:8080/v1/runs?wait=true'
```

### Python

```python
import json, os, requests

BASE, H = "http://localhost:8080", {"Authorization": "Bearer " + os.environ["THINKING_CORE_TOKEN"]}

def ask_agent(message, session_id=None):
    run = requests.post(f"{BASE}/v1/runs", headers=H,
                        json={"message": message, "sessionId": session_id}).json()
    with requests.get(f"{BASE}/v1/runs/{run['runId']}/events", headers=H, stream=True) as r:
        event = None
        for line in r.iter_lines(decode_unicode=True):
            if line.startswith("event: "):
                event = line[7:]
            elif line.startswith("data: "):
                data = json.loads(line[6:])
                if event == "input_request":   # the agent called ask()
                    requests.post(f"{BASE}/v1/runs/{run['runId']}/answer", headers=H,
                                  json={"requestId": data["args"]["requestId"], "value": input(data["content"] + " ")})
                if event == "done":
                    break
    return requests.get(f"{BASE}/v1/runs/{run['runId']}", headers=H).json()
```
