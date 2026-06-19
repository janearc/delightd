# delightd — agent interface

delightd is built to be driven by agents, not just humans. The contract is:

> A capability is not done until it emits JSON and has a wrapper and a skill.

Concretely, every delightd capability is reachable three ways that all speak the
same data: a JSON HTTP endpoint, a `delight` CLI subcommand, and an MCP tool. This
document covers the agent-facing half — the skill aggregator, the generated CLI,
the registry, and the reload path. The raw HTTP surface is in [api.md](api.md).

## JSON by default

Every control-port response is `application/json` (see [api.md](api.md)). There
is no HTML surface and no human-only output mode on the daemon. An agent reads
`/git`, `/state`, `/introspect`, `/discovery/llms`, and the MCP tool list and
gets structured data in every case.

## The skill aggregator

delightd does not just expose its own tools — it **aggregates the fleet's**. On
each sync the aggregator (`pkg/skills`) scans every managed project for an
`mcp.json` at the project root and registers the tools it declares.

- **Discovery.** For each project, read `<workDir>/<project>/mcp.json`. A project
  without one is skipped silently; an unreadable or malformed one is logged and
  skipped (one bad file never breaks the aggregate).
- **Namespacing.** Each tool is registered as `<project>_<tool>` to avoid
  collisions across projects. A `paling` project declaring `train` becomes
  `paling_train`.
- **Dogfood tool.** The aggregator always injects `delightd_trigger_backup`
  (input `{ "project": "<name>" }`) so the daemon's own backup trigger is part of
  the same surface it serves for everyone else.

A project's `mcp.json` is the single integration contract for its agent tools.
Nothing else is scanned — there is no `swagger.json` or OpenAPI requirement.

`mcp.json` shape (`pkg/skills.Manifest`):

```json
{
  "tools": [
    {
      "name": "train",
      "description": "Kick off a training run.",
      "inputSchema": { "type": "object", "properties": { "config": { "type": "string" } } },
      "handler": { "type": "command", "command": "/usr/local/bin/paling", "args": ["train"] }
    }
  ]
}
```

| `handler.type` | Dispatch |
|----------------|----------|
| `command` | exec `command` with `args` (+ caller args in the CLI) |
| `http` | `curl -X <method> <url>` |
| `internal` | daemon-internal (e.g. the `backup` method) |

## MCP server (`POST /mcp`)

The aggregated tools are served over MCP (JSON-RPC 2.0) at `POST /mcp`, enabled
when `system.agent_skills.enabled` is true and `expose_via` contains `"mcp"`.

- `tools/list` → every aggregated tool with its `inputSchema`.
- `tools/call` → dispatch one tool by its namespaced name.

Request/response shapes and error codes are in [api.md](api.md#post-mcp). This is
the path an MCP-capable agent uses to enumerate and invoke fleet tools through a
single endpoint.

## The `delight` CLI wrapper

When `expose_via` contains `"cli"`, the daemon **generates** a bash router at
`~/var/bin/delight` (override the directory with `DELIGHT_EXPORTS_BIN`). It is
regenerated on every skill sync, so it always reflects the current aggregate.

```bash
delight <project> <action> [args...]

# trigger a checkpoint via the injected dogfood tool
delight delightd trigger_backup paling
```

The wrapper is a generated `case` over `<project>_<action>`: a `command` tool
execs its command, an `http` tool curls its URL, and the internal `backup`
method posts to `http://localhost:8088/projects/<arg>/backup`. With no matching
subcommand it prints usage listing every available `delight <project> <action>`.

## The registry and reload

Two distinct config inputs feed the agent/exports surface:

| File | Role |
|------|------|
| `delight.yaml` | the daemon config: projects, ports, agent_skills toggles, kafka |
| `delight-registry.yaml` | the **exports registry**: declares docker-backed tools the exports engine wraps |

The exports engine reads the registry at `DELIGHT_EXPORTS_REGISTRY` (code default
`~/etc/delight-registry.yaml`; the kube deployment sets
`/etc/delightd/delight-registry.yaml`). From it the daemon generates docker shim
scripts (`<bin>.sh`) under the exports state dir and symlinks them into
`~/var/bin`, alongside static binaries it finds in `~/work/<project>/bin/`.

> Path note. The code default registry path is `~/etc/delight-registry.yaml`; the
> deployed path under kube is `/etc/delightd/delight-registry.yaml`. These differ;
> set `DELIGHT_EXPORTS_REGISTRY` to pin the one you mean. See
> [operations.md](operations.md) for the full env table.

**Reload.** The daemon re-syncs exports and skills on a **5 minute** loop, so a
new `mcp.json`, a new registry entry, or a new project binary is picked up within
that interval without a restart — the CLI and MCP surface regenerate from the
fresh scan each cycle. There is no separate signal-driven reload today; the
periodic sync is the reload path.

## Putting it together: a new capability

To add a capability the agent-first way:

1. The capability emits JSON on a stable endpoint (its own service, or a delightd
   route).
2. Declare it in the project's `mcp.json` (namespaced automatically to
   `<project>_<tool>`), or register a docker-backed tool in
   `delight-registry.yaml`.
3. delightd picks it up on the next sync: it appears in `tools/list` over MCP and
   as a `delight <project> <tool>` CLI subcommand.

Until all three exist — JSON, wrapper, skill — the capability is not done.
