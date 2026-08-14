# tln-plugin

[![CI](https://github.com/opentalon/tln-plugin/actions/workflows/ci.yml/badge.svg)](https://github.com/opentalon/tln-plugin/actions/workflows/ci.yml)

OpenTalon plugin that executes [Tln](https://github.com/opentalon/tln-language) workflow blocks emitted by the LLM. Use it for deterministic multi-step MCP chains — batch operations, paginated fetches, multi-tool dependencies — where the agent loop is too lossy or too slow.

## How it works

1. The LLM sees the `tln-plugin.execute_workflow(workflow)` tool plus a system-prompt addition explaining the Tln DSL. A companion `tln-plugin.check(workflow)` action validates source without executing it (`tln.Check`) — it returns `{"ok":true}` or the compile diagnostics, so any caller authoring machine-generated Tln can validate before storing or running. A third action, `tln-plugin.evaluate(source, facts, snapshot)`, reactively evaluates source against facts: it hydrates a `Session` from the prior snapshot, asserts the new facts, fires any matching `on`-blocks (running their workflow bodies through the host), and returns the firings plus the updated snapshot — a stateless primitive for running event-driven watchers one tick at a time.
2. For a batch request ("delete all Test items"), the LLM emits a single tool call whose argument is a Tln `workflow "..." { step "..." { mcp "..." "..." { ... } } }` block.
3. The host dispatches the call to this plugin over the existing plugin gRPC connection using **bidirectional streaming** (`ExecuteBidi`) — the same Unix socket the host already uses to call the plugin, no new transport.
4. The plugin compiles the workflow via [`tln-language/pkg/tln.RunWorkflow`](https://github.com/opentalon/tln-language/tree/master/pkg/tln) and runs it.
5. For each `mcp "<server>" "<tool>" {...}` step the workflow asks the plugin to execute, the plugin **calls back** to the host over the same stream. The host's orchestrator runs the call through its normal `executeCall` path — full policy, observability, credential injection, and usage tracking. The plugin never talks to MCP servers directly.
6. The plugin assembles the workflow result and returns a single `ToolResultResponse` to the LLM with a human-readable summary plus a JSON `structured_content` blob containing the per-step trace.

## Requirements

- **OpenTalon host >= v0.0.18** — the bidi `ExecuteBidi` RPC and the `supports_callbacks` capability flag were added in that release.
- **tln-language >= v0.5.1** — `pkg/tln.Run` / `RunWorkflow` / `Check` + the `FactStore` interface.
- For Datalevin-backed programs (`detect`, queries, ML primitives): a reachable [datalevin-server](https://github.com/opentalon/tln-language/tree/master/datalevin-server). Without one, the plugin still runs workflow-only programs.

## Config

```yaml
plugins:
  tln:                                        # config-map key — also the reverse-proxy URL path
    enabled: true
    github: "opentalon/tln-plugin"
    ref: "master"
    expose_http: true   # required only if admin_token is set (see below)
    config:
      datalevin_url: "http://localhost:8898"   # optional; enables detect/query/ML programs
      rules_dir: "/etc/opentalon/tln-rules"  # required if admin_token is set
      admin_token: "${TLN_PLUGIN_ADMIN_TOKEN}"  # bearer token guarding the admin HTTP API
```

The host auto-fetches, builds, and pins the binary via `plugins.lock`. When `expose_http: true` is set on the entry, the host reverse-proxies `/{config-map-key}/*` from its webhook server to the plugin's HTTP listener — so the key `tln:` above gives you `/tln/*`. The capability name advertised to the LLM stays `tln-plugin` (`tln-plugin.execute_workflow`); operators pick any URL-friendly key they want.

## Admin HTTP API

When `admin_token` is set in the config block and the host has granted HTTP (`expose_http: true`), the plugin starts a management HTTP server on `OPENTLN_HTTP_PORT`. Every request requires `Authorization: Bearer <admin_token>`; missing or wrong tokens return `401`.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/rules` | List all rule names. |
| `POST` | `/rules` | Create a rule. Body: `{"name": "...", "source": "<Tln>"}`. Validates the source via the SDK before writing; invalid Tln → `400`. |
| `GET` | `/rules/{name}` | Fetch the rule's Tln source (text/plain). |
| `PUT` | `/rules/{name}` | Replace a rule. Body is the Tln source as text. |
| `DELETE` | `/rules/{name}` | Remove a rule. |

Rules are filesystem-backed in `rules_dir` — one `<name>.tln` file per rule. Names must match `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$` so they can't escape the directory via `../` or land on awkward paths.

Curl example (host's webhook at `https://opentalon.example.com`):

```
curl -X POST https://opentalon.example.com/tln/rules \
  -H "Authorization: Bearer $TLN_PLUGIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"fleet_maintenance","source":"workflow \"x\" { ... }"}'
```

Setting `admin_token` without `expose_http: true` (or vice versa) is a misconfiguration — the plugin logs a warning at startup and refuses to serve an auth-less API.

### Facts API (one-at-a-time CRUD)

When `datalevin_url` is configured, the admin server also exposes per-entity fact CRUD. Same bearer-token auth as `/rules`.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/facts` | Add a fact. Body: `{"entity_id": <int>, "attrs": {"k1": v1, "k2": v2}}`. Issues a single `Transact` against the FactStore. |
| `GET` | `/facts/{id}` | Read all `(attr, value)` pairs on an entity. Returns `{"entity_id": ..., "attrs": {...}}` or `404`. |
| `PUT` | `/facts/{id}` | Patch attrs on an entity. Body: `{"attrs": {...}}`. Datalevin treats re-asserting an attribute as an update — no retract dance needed. |
| `DELETE` | `/facts/{id}` | Retract the whole entity. |
| `DELETE` | `/facts/{id}/{attr}` | Retract a single attribute (looks up current value, then issues `:db/retract`). Returns `404` if the attribute isn't set. |

Curl examples:

```
# Add a stock item
curl -X POST https://opentalon.example.com/tln/facts \
  -H "Authorization: Bearer $TLN_PLUGIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"entity_id": 808, "attrs": {"name": "Cement 50kg", "current_stock": 12, "minimum_amount": 50}}'

# Patch stock level
curl -X PUT https://opentalon.example.com/tln/facts/808 \
  -H "Authorization: Bearer $TLN_PLUGIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"attrs": {"current_stock": 8}}'

# Read current state
curl https://opentalon.example.com/tln/facts/808 \
  -H "Authorization: Bearer $TLN_PLUGIN_ADMIN_TOKEN"

# Retract one attribute
curl -X DELETE https://opentalon.example.com/tln/facts/808/current_stock \
  -H "Authorization: Bearer $TLN_PLUGIN_ADMIN_TOKEN"
```

Without `datalevin_url`, every `/facts/*` request returns `503` — admins seeding facts and the LLM running detect rules share the same backend by construction, so a missing backend is a setup problem to surface rather than a silent no-op.

### Not yet exposed

- Bulk seed from a `.tln.test` source (`POST /facts/seed`) — straightforward follow-up using `tln.Seed`.
- Rule execution as advertised actions (one Action per `.tln` file). Needs an SDK-side `WithContext(map[string]any)` option to bind LLM params into Tln's `context.*` lookups — separate tln-language PR.

Workflow-only mode (no `datalevin_url`) is the default — the plugin runs `tln.RunWorkflow` and rejects `detect`-bearing programs with a clear error pointing at the missing backend. Setting `datalevin_url` switches to `tln.Run` for the full language.

## Spec

| Item | Value |
|------|--------|
| **Plugin ID** | `tln-plugin` |
| **Action** | `execute_workflow` |
| **Streaming** | Yes (`supports_callbacks: true` → host dispatches over `ExecuteBidi`) |

**Parameters**

| Name | Type | Required | Description |
|------|------|----------|-------------|
| `workflow` | string | yes | A Tln `workflow "..." { ... }` block. See [`workflow_tool.txt`](./workflow_tool.txt) for the DSL syntax and examples. |

## Build

```
go build -o tln-plugin .
```

For production deployments, prefer `github:` auto-fetch (see Config above) — the host clones, builds, and pins the binary via `plugins.lock`, so operators don't manage release artifacts manually.

## Scope today

- **Workflow blocks**: ad-hoc LLM-authored Tln workflows (`workflow "..." { step "..." { mcp ... } }`) run via `tln.RunWorkflow`. No backend dependency.
- **Detect / query / ML primitives**: covered when `datalevin_url` is configured — programs flow through `tln.Run` against the Datalevin store.

**Not yet shipped** (follow-up work in this repo):

- A rules-directory loader exposing one advertised action per preauthored `.tln` rule file. Today only the generic `execute_workflow(workflow)` tool is exposed; rule authoring is a future surface that needs an SDK-side `WithContext(map[string]any)` option to bind LLM params into Tln's `context.*` lookups.

## Tests

```
go test -race -count=1 ./...
```

Covers: workflow execution, step-result chaining (`step("name").result.field`), host-error propagation through the tln runtime, missing/unknown action handling, and a unary-path refusal guard (in case the host hasn't been upgraded to bidi).
