# delightd: Integration Invariants

`delightd` orchestrates backups, exports host binaries, and aggregates MCP tools. Projects integrated into the fleet must conform to specific hyperscaler architectural constraints.

## Eligibility Constraints

The daemon enforces the following baseline criteria. Violations disqualify a project from managed integration.

### Required
- Containerized execution model.
- HTTP control port exposing `/health` and `/metrics` (strongly recommended).
- Ephemeral build structures with zero persistent side-effects.
- Unified AI tooling specifications (`mcp.json` or OpenAPI `swagger.json` at root).

### Disqualifications
- Unbounded file system mutations dropping `/` or `~` below 50GB free space.
- Unsandboxed `/usr/local` or `/usr/bin` writes.
- Network assets requiring >6h fetch times without explicit human approval.
- Deeply nested conditional logic replacing explicit Context (`ctx`) pipelines.

## Binary Export Invariants

The daemon manages the host `$PATH` via `~/var/bin` (which is configurable ofc).

### Static Binaries
Compiled binaries are expected to be located in `~/work/<project>/bin/`. The daemon will automatically manage symlink lifecycle for these paths.

### Docker Executables
Container-bound tools must be declared in `~/etc/delightd/delight-registry.yaml`. The daemon will automatically generate lightweight bash wrapper scripts to proxy local execution into isolated Docker containers.

## Tool Aggregation (MCP)

The daemon consumes project-level tooling specifications and multiplexes them for autonomous orchestration.

### Definition
Projects must define their execution footprint as a Docker container. While `mcp.json` is useful for Agent routing, the hard requirement is simply providing endpoints that register with the Traefik or Envoy service mesh. The daemon aggregates these specifications for discovery.

### Execution Interfaces

#### 1. Unified CLI Wrapper (`delight`)
The daemon emits an AWS-style CLI wrapper at `~/var/bin/delight`.

```bash
# General syntax
delight <project> <tool> [args...]

# Example: Force trigger a checkpoint
delight delightd trigger_backup odysseus
```

#### 2. Model Context Protocol (MCP) Server
The daemon serves the aggregated schema over a JSON-RPC interface.

```http
POST http://localhost:8088/mcp
```
- `tools/list`: Yields the aggregated fleet tool schema.
- `tools/call`: Dispatches execution to the corresponding project handler. No custom middleware required.
