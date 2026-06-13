# delightd: Developer Integration Guide

`delightd` acts as the central nervous system for your fleet. It autonomously manages backups, exports your CLI tools to `$PATH`, and aggregates your Agent Skills for AI consumption. 

To integrate your project with `delightd`, your project must follow the Hyperscaler Mindset (Cattle, not pets).

## 1. Project Eligibility Checklist

Before `delightd` will fully embrace your project, you must meet the following baseline requirements:

### ✅ Required (Eligible Citizen)
- [ ] **Decoupled Architecture**: Container-first execution model.
- [ ] **Observability**: Exposes a `/health` and/or `/metrics` endpoint on a control port.
- [ ] **Stateless Builds**: Can be built and torn down cleanly.
- [ ] **Agent Tooling**: Defines an `mcp.json` or standard OpenAPI spec (`swagger.json`) at its root.

### ❌ Disqualifying (Anti-Patterns)
- [ ] **Monolithic State**: Fails to use Context (`ctx`) or relies heavily on deeply nested if/else spaghetti.
- [ ] **Root Mutations**: Executes actions that drop `/` or `~` below 50GB free space, or attempts to write to `/usr/local` or `/usr/bin`.
- [ ] **Long-Blocking Assets**: Requires >6h to download cache dependencies without explicit human approval.

---

## 2. Exporting Binaries & CLI Tools

`delightd` automatically brings your project's CLI tools into the engineer's `$PATH` via `~/var/bin`.

### Sensible Defaults (Zero-Touch)
If you compile native binaries, simply put them in `bin/` inside your project root (`~/work/<your-project>/bin/`). `delightd` will automatically symlink them into `~/var/bin`.

### Docker Wrappers
If your CLI tool requires Docker execution (e.g. `docker run` or `docker exec`), define it in the central registry at `~/etc/delightd/delight-registry.yaml`. `delightd` will generate the Bash wrapper for you automatically.

---

## 3. Aggregating Agent Skills

AI agents (like Claude or Antigravity) use `delightd` to discover what your project can do. 

`delightd` exposes a **Model Context Protocol (MCP)** server on its control port. When an agent connects to it, `delightd` serves the agent every tool defined by your project.

### How to expose your skills:
Instead of writing custom YAML logic, simply drop a standard `mcp.json` file in your project root, or expose an OpenAPI specification via Traefik. 

`delightd` parses these industry-standard footprints, aggregates them, and additionally exposes them via the Unified CLI pattern (`delight <project> <action>`).

### Utilizing Agent Skills (Testing/Usage)

Once your project exposes an `mcp.json` file, `delightd` will aggregate your tools. You can interact with these skills in two ways:

#### A. The Unified CLI (`delight`)
`delightd` generates a unified AWS-style CLI router at `~/var/bin/delight`. You can use this to execute any registered tool natively from your terminal:

```bash
# General syntax
delight <project_name> <tool_name> [args...]

# Example: Dogfooding backup trigger
delight delightd trigger_backup odysseus
```
*(Running `delight` with no arguments will dump the full list of available tools across your fleet).*

#### B. Model Context Protocol (MCP) Server
If you're using an AI assistant (like Claude, Cursor, or an autonomous agent), you can point it to `delightd`'s local MCP endpoint. The AI will ingest the entire fleet's tool schema dynamically:

```http
POST http://localhost:8088/mcp
```
- **`tools/list`**: Fetches the fleet registry.
- **`tools/call`**: Invokes the specific tool via its JSON-RPC spec. No custom middleware required!
