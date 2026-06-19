# delightd — HTTP API reference

Every route is served on the control port (`:8088`, canonical) by `pkg/httpapi`.
All responses are `application/json`. The route table below is the complete
surface registered in `Mux()`.

| Method | Path | Handler purpose |
|--------|------|-----------------|
| GET | `/health` | liveness + active project count |
| GET | `/metrics` | prometheus exposition |
| GET | `/discovery/llms` | currently discoverable local LLM endpoints |
| GET | `/git` | live git state for every managed project |
| GET | `/projects/{name}/git` | live git state for one project |
| GET | `/projects/{name}/state` | backup state-machine diagnostics |
| GET | `/projects/{name}/introspect` | known / backing-up / has-fragment view |
| POST | `/projects/{name}/backup` | manually trigger a checkpoint |
| POST | `/projects/{name}/reset` | clear a stuck error state |
| POST | `/mcp` | agent skill aggregator (MCP JSON-RPC); only when MCP is enabled |

`/mcp` is registered only when `system.agent_skills.enabled` is true **and**
`system.agent_skills.expose_via` contains `"mcp"`. When disabled, the route does
not exist and a request returns 404 from the mux.

Two distinct 404 semantics apply across the surface, and the difference is
deliberate:

- **Unknown project, control/state routes** (`/state`, `/backup`, `/reset`,
  `/projects/{name}/git`) → `404` with `{"error": "project not found"}`. These
  act on a project; an unknown name has no machine to act on.
- **Unknown project, introspection** (`/introspect`) → `200` with
  `is_known_to_daemon: false`. Introspection is a *query about whether the
  daemon knows a project*; "no" is a valid answer, not an error.

---

## GET /health

Liveness probe and a count of managed projects.

```json
{ "status": "ok", "active_projects": 3, "dry_run": false }
```

| Field | Meaning |
|-------|---------|
| `status` | always `"ok"` when the handler runs |
| `active_projects` | number of projects in the loaded config |
| `dry_run` | whether the daemon was started with `--dry-run` |

Status: always `200`. This is the readiness/liveness target in the kube probes.

## GET /metrics

Prometheus text exposition (`pkg/metrics`). Counters include
`delightd_git_churn_checks_total{project}`,
`delightd_backup_success_total{project}`, and
`delightd_backup_failures_total{project}`. Status: `200`.

## GET /discovery/llms

The local LLMs delightd currently sees. Each source is one probed provider.

```json
{
  "status": "ok",
  "sources": [
    {
      "provider": "ollama",
      "url": "http://localhost:11434",
      "models": ["llama3.1:8b"],
      "healthy": true
    }
  ]
}
```

| Field | Meaning |
|-------|---------|
| `sources[].provider` | provider name (from config, or a standard default) |
| `sources[].url` | endpoint probed |
| `sources[].models` | model identifiers reported by the provider |
| `sources[].healthy` | whether the probe succeeded |

Discovery runs against configured providers (`system.llm_discovery.providers`,
types `ollama`, `llama_cpp`, `openai`, `apfel`) or standard local ports. The
daemon also syncs discovered endpoints into traefik on a 30 s loop; this
endpoint reports the same view on demand. Status: always `200` (an empty
`sources` array means nothing was reachable).

## GET /git

Live git state for **every** managed project, returned under `projects`.

```json
{
  "status": "ok",
  "projects": [
    {
      "name": "paling",
      "git": {
        "branch": "main",
        "dirty": false,
        "unpushed": 0,
        "has_upstream": true,
        "remote_url": "git@github.com:janearc/paling.git"
      }
    }
  ]
}
```

`git` field semantics are identical to the per-project route below.

**Sweep behavior (load-bearing).** Projects are read **concurrently** with a
bound of 8 in-flight reads, each under a **5 s per-project deadline**.

- A serial sweep would make the total cost the sum of every project's read, so
  one slow tree could time out the whole endpoint — and fleet's `git status`,
  which fails closed on this endpoint, with it.
- go-git's calls take no `context`, so a slow read cannot be cancelled. The
  sweep stops *waiting* on it at the deadline and reports
  `git.error: "git state read exceeded 5s"` for that project; the orphaned read
  finishes on its own.
- A failure or timeout on one project is reported in that project's `git.error`.
  It **never** aborts the sweep — the other projects still return.
- Output is sorted by project name for stable responses.

Status: always `200`. Per-project failures live in-band in `git.error`, not in
the HTTP status. The handler logs each `git.error` (the `gitstate` package
itself never logs; surfacing is the handler's half of the contract).

## GET /projects/{name}/git

Live git state for one project.

```json
{
  "name": "paling",
  "git": {
    "branch": "main",
    "dirty": false,
    "unpushed": 0,
    "has_upstream": true,
    "remote_url": "git@github.com:janearc/paling.git"
  }
}
```

| Field | Meaning |
|-------|---------|
| `git.branch` | current branch (empty in a detached HEAD) |
| `git.dirty` | working tree has uncommitted changes (tracked or untracked) |
| `git.unpushed` | commits reachable from `HEAD` not on the tracking ref |
| `git.has_upstream` | a tracking ref exists; when `false`, every local commit counts as unpushed |
| `git.remote_url` | tracking remote URL (branch upstream → `origin` → sole remote) |
| `git.error` | present only on read failure; other fields hold zero values |

The remote is resolved rather than assumed `origin`: fleet projects vary (some
name the remote `github`), so a hardcoded `origin` would report everything as
never-pushed.

Status: `200` for a known project (including the case where the read failed and
`git.error` is set); `404` `{"error": "project not found"}` for an unknown name.

## GET /projects/{name}/state

Backup state-machine diagnostics for a project.

```json
{
  "state": "monitoring",
  "error_count": 0,
  "last_activity": "2026-06-19T10:04:00Z",
  "next_retry": "0001-01-01T00:00:00Z"
}
```

| Field | Meaning |
|-------|---------|
| `state` | `fallow` \| `monitoring` \| `backing_up` \| `error` |
| `error_count` | consecutive backup failures |
| `last_activity` | last state-machine activity timestamp |
| `next_retry` | when an `error`-state machine may retry (zero value when not in error) |

Status: `200` for a known project; `404` `{"error": "project not found"}`
otherwise.

## GET /projects/{name}/introspect

The daemon's introspection view of a project.

```json
{
  "service_name": "paling",
  "is_known_to_daemon": true,
  "is_actively_backing_up": false,
  "has_bash_fragment": true
}
```

| Field | Meaning |
|-------|---------|
| `service_name` | the queried name (echoed back) |
| `is_known_to_daemon` | the project is present in the daemon's config |
| `is_actively_backing_up` | the project's state machine is currently `backing_up` |
| `has_bash_fragment` | at least one generated docker shim exists for the project |

> Pending wire rename. The type is `ServiceBackupStatus` and the fields are
> `service_name` / `is_known_to_daemon`. This predates the
> [project taxonomy](../delightd_architecture.md#6-taxonomy-what-is-a-project)
> and is slated to rename to `project`. The shape above is what the wire returns
> today; treat the names as transitional. Field names mirror
> `delight.v1.ServiceBackupStatus` so the surface graduates to Protobuf cleanly.

**Status: always `200`.** An unknown project returns `200` with
`is_known_to_daemon: false`, not `404`. "The daemon has never heard of this
project" is a valid, queryable answer worth recording as a signal, not an error.

## POST /projects/{name}/backup

Manually trigger a checkpoint by driving the state machine to `backing_up`.

```json
{ "status": "backup_triggered", "project": "paling" }
```

| Status | Condition |
|--------|-----------|
| `200` | transition accepted; `{"status": "backup_triggered", "project": "<name>"}` |
| `404` | unknown project; `{"error": "project not found"}` |
| `409` | the machine could not transition (e.g. already backing up); `{"error": "<reason>"}` |

The actual checkpoint runs on the per-project eval loop once the machine is in
`backing_up`; this endpoint requests the transition, it does not block on the
tarball.

## POST /projects/{name}/reset

Clear a stuck `error` state, returning the machine toward `fallow`.

```json
{ "status": "error_cleared", "project": "paling" }
```

| Status | Condition |
|--------|-----------|
| `200` | error cleared; `{"status": "error_cleared", "project": "<name>"}` |
| `404` | unknown project; `{"error": "project not found"}` |
| `409` | the clear transition was rejected; `{"error": "<reason>"}` |

## POST /mcp

JSON-RPC 2.0 endpoint for the Model Context Protocol — the aggregated agent-tool
surface. Registered only when MCP exposure is enabled (see top of this doc).

`tools/list` returns every aggregated tool:

```json
{ "jsonrpc": "2.0", "id": 1, "method": "tools/list" }
```

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": {
    "tools": [
      {
        "name": "delightd_trigger_backup",
        "description": "Manually trigger an immediate backup for a specific project.",
        "inputSchema": { "type": "object", "properties": { "project": { "type": "string" } }, "required": ["project"] }
      }
    ]
  }
}
```

`tools/call` dispatches one tool by its namespaced name:

```json
{ "jsonrpc": "2.0", "id": 2, "method": "tools/call",
  "params": { "name": "delightd_trigger_backup", "arguments": { "project": "paling" } } }
```

| JSON-RPC error code | Condition |
|---------------------|-----------|
| `-32601` (method not found) | unknown `method`, or `tools/call` for an unknown tool name |
| `-32602` (invalid params) | malformed `params` on `tools/call` |

A non-POST request to `/mcp` returns HTTP `405`. Tool discovery, namespacing,
and the generated `delight` CLI are described in
[agent-interface.md](agent-interface.md).

---

See also: [availability.md](availability.md) (why `/git` is computed live and
fails closed for consumers), [events.md](events.md) (the Kafka surface, which is
not an HTTP route).
