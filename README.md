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
- The host plugin loader does the rest — no special config beyond installing the plugin binary.

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
make build
```

Produces a `talon-plugin` binary. Install it like any other OpenTalon plugin (point your `plugins:` entry at the binary path, or use `github:` auto-fetch).

## Scope today

- **Workflow blocks only.** Talon programs that use `detect` over a fact store need the Datalevin-backed entry point in `talon-language/pkg/talon`, which is a follow-up. Until then this plugin handles the ad-hoc batch case; preauthored EITL rules over fact stores arrive in a later release.

## Tests

```
make test
```

Covers: workflow execution, step-result chaining (`step("name").result.field`), host-error propagation through the talon runtime, missing/unknown action handling, and a unary-path refusal guard (in case the host hasn't been upgraded to bidi).
