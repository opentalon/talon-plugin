# talon-plugin

[![CI](https://github.com/opentalon/talon-plugin/actions/workflows/ci.yml/badge.svg)](https://github.com/opentalon/talon-plugin/actions/workflows/ci.yml)

OpenTalon plugin that executes [Talon](https://github.com/opentalon/talon-language) workflow blocks emitted by the LLM. Use it for deterministic multi-step MCP chains — batch operations, paginated fetches, multi-tool dependencies — where the agent loop is too lossy or too slow.

## How it works

1. The LLM sees the `talon-plugin.execute_workflow(workflow)` tool plus a system-prompt addition explaining the Talon DSL.
2. For a batch request ("delete all Test items"), the LLM emits a single tool call whose argument is a Talon `workflow "..." { step "..." { mcp "..." "..." { ... } } }` block.
3. The host dispatches the call to this plugin over the existing plugin gRPC connection using **bidirectional streaming** (`ExecuteBidi`) — the same Unix socket the host already uses to call the plugin, no new transport.
4. The plugin compiles the workflow via [`talon-language/pkg/talon.RunWorkflow`](https://github.com/opentalon/talon-language/tree/master/pkg/talon) and runs it.
5. For each `mcp "<server>" "<tool>" {...}` step the workflow asks the plugin to execute, the plugin **calls back** to the host over the same stream. The host's orchestrator runs the call through its normal `executeCall` path — full policy, observability, credential injection, and usage tracking. The plugin never talks to MCP servers directly.
6. The plugin assembles the workflow result and returns a single `ToolResultResponse` to the LLM with a human-readable summary plus a JSON `structured_content` blob containing the per-step trace.

## Requirements

- **OpenTalon host >= v0.0.18** — the bidi `ExecuteBidi` RPC and the `supports_callbacks` capability flag were added in that release.
- **talon-language >= v0.2.0** — `pkg/talon.Run` + the `FactStore` interface.
- For Datalevin-backed programs (`detect`, queries, ML primitives): a reachable [datalevin-server](https://github.com/opentalon/talon-language/tree/master/datalevin-server). Without one, the plugin still runs workflow-only programs.

## Config

```yaml
plugins:
  talon:                                        # config-map key — also the reverse-proxy URL path
    enabled: true
    github: "opentalon/talon-plugin"
    ref: "master"
    expose_http: true   # required only if admin_token is set (see below)
    config:
      datalevin_url: "http://localhost:8898"   # optional; enables detect/query/ML programs
      rules_dir: "/etc/opentalon/talon-rules"  # required if admin_token is set
      admin_token: "${TALON_PLUGIN_ADMIN_TOKEN}"  # bearer token guarding the admin HTTP API
```

The host auto-fetches, builds, and pins the binary via `plugins.lock`. When `expose_http: true` is set on the entry, the host reverse-proxies `/{config-map-key}/*` from its webhook server to the plugin's HTTP listener — so the key `talon:` above gives you `/talon/*`. The capability name advertised to the LLM stays `talon-plugin` (`talon-plugin.execute_workflow`); operators pick any URL-friendly key they want.

## Admin HTTP API

When `admin_token` is set in the config block and the host has granted HTTP (`expose_http: true`), the plugin starts a management HTTP server on `OPENTALON_HTTP_PORT`. Every request requires `Authorization: Bearer <admin_token>`; missing or wrong tokens return `401`.

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/rules` | List all rule names. |
| `POST` | `/rules` | Create a rule. Body: `{"name": "...", "source": "<Talon>"}`. Validates the source via the SDK before writing; invalid Talon → `400`. |
| `GET` | `/rules/{name}` | Fetch the rule's Talon source (text/plain). |
| `PUT` | `/rules/{name}` | Replace a rule. Body is the Talon source as text. |
| `DELETE` | `/rules/{name}` | Remove a rule. |

Rules are filesystem-backed in `rules_dir` — one `<name>.talon` file per rule. Names must match `^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$` so they can't escape the directory via `../` or land on awkward paths.

Curl example (host's webhook at `https://opentalon.example.com`):

```
curl -X POST https://opentalon.example.com/talon/rules \
  -H "Authorization: Bearer $TALON_PLUGIN_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"name":"fleet_maintenance","source":"workflow \"x\" { ... }"}'
```

Setting `admin_token` without `expose_http: true` (or vice versa) is a misconfiguration — the plugin logs a warning at startup and refuses to serve an auth-less API.

### Not yet exposed

`POST /facts/seed` (push facts into the Datalevin store via `talon.Seed`) follows once the talon-language SDK adds a public `FactStore` constructor — small follow-up PR. Rule execution against loaded rules (one advertised action per rule) follows after that.

Workflow-only mode (no `datalevin_url`) is the default — the plugin runs `talon.RunWorkflow` and rejects `detect`-bearing programs with a clear error pointing at the missing backend. Setting `datalevin_url` switches to `talon.Run` for the full language.

## Spec

| Item | Value |
|------|--------|
| **Plugin ID** | `talon-plugin` |
| **Action** | `execute_workflow` |
| **Streaming** | Yes (`supports_callbacks: true` → host dispatches over `ExecuteBidi`) |

**Parameters**

| Name | Type | Required | Description |
|------|------|----------|-------------|
| `workflow` | string | yes | A Talon `workflow "..." { ... }` block. See [`workflow_tool.txt`](./workflow_tool.txt) for the DSL syntax and examples. |

## Build

```
go build -o talon-plugin .
```

For production deployments, prefer `github:` auto-fetch (see Config above) — the host clones, builds, and pins the binary via `plugins.lock`, so operators don't manage release artifacts manually.

## Scope today

- **Workflow blocks**: ad-hoc LLM-authored Talon workflows (`workflow "..." { step "..." { mcp ... } }`) run via `talon.RunWorkflow`. No backend dependency.
- **Detect / query / ML primitives**: covered when `datalevin_url` is configured — programs flow through `talon.Run` against the Datalevin store.

**Not yet shipped** (follow-up work in this repo):

- A rules-directory loader exposing one advertised action per preauthored `.talon` rule file. Today only the generic `execute_workflow(workflow)` tool is exposed; rule authoring is a future surface that needs an SDK-side `WithContext(map[string]any)` option to bind LLM params into Talon's `context.*` lookups.

## Tests

```
go test -race -count=1 ./...
```

Covers: workflow execution, step-result chaining (`step("name").result.field`), host-error propagation through the talon runtime, missing/unknown action handling, and a unary-path refusal guard (in case the host hasn't been upgraded to bidi).
