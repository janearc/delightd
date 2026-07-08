# delightd — HTTP API reference

Every route is served on the control port (`:8088`, canonical) by `pkg/httpapi`.
All responses are `application/json`. The route table below is the complete
surface registered in `Mux()`.

| Method | Path | Handler purpose |
|--------|------|-----------------|
| GET | `/health` | liveness + active project count |
| GET | `/readyz` | provable readiness: roots mounted + kubectl reachable |
| GET | `/metrics` | prometheus exposition |
| GET | `/discovery/llms` | currently discoverable local LLM endpoints |
| GET | `/projects` | authoritative roster (name/path/essential/deploy/remote_url) for all managed projects |
| GET | `/git` | live git state for every managed project |
| GET | `/projects/{name}/git` | live git state for one project |
| GET | `/projects/{name}/state` | backup state-machine diagnostics |
| GET | `/state` | enablement home: every project's effective enable/disable state |
| GET | `/state/{name}` | one project's enablement state |
| PUT | `/state/{name}` | idempotent enable/disable write (roster-bound) |
| GET | `/projects/{name}/introspect` | known / backing-up / has-fragment view |
| POST | `/projects/{name}/backup` | manually trigger a checkpoint |
| POST | `/projects/{name}/reset` | clear a stuck error state |
| POST | `/register` | a frood joins the live registry (additive, optional; not yet required) |
| GET | `/registrations` | live frood registrations (`registry.v1.RegistrationSet`), alongside the static roster |
| GET | `/resolve/{name}` | narrow widget-facing resolution (`resolve.v1.ResolvedService`): scheme + address for one project |
| GET | `/services` | composed roster (entity-query list), optional `?type=` filter |
| GET | `/services/{name}` | one composed roster entry, facets as fields |
| POST | `/mcp` | agent skill aggregator (MCP JSON-RPC); only when MCP is enabled |

`/mcp` is registered only when `system.agent_skills.enabled` is true **and**
`system.agent_skills.expose_via` contains `"mcp"`. When disabled, the route does
not exist and a request returns 404 from the mux.

The word "state" appears in two route families that do different jobs.
`/state` and `/state/{name}` are enablement: the fleet-wide enable/disable
home. `/projects/{name}/state` is unrelated: it reports the backup state
machine for one project. When a path starts with `/state`, it is always
enablement.

The status semantics across the surface differ deliberately:

- **Unknown project, control/state routes** (`/projects/{name}/state`,
  `/backup`, `/reset`, `/projects/{name}/git`, `/services/{name}`, and the
  enablement routes at `/state/{name}`) → `404` with an `error` body. These
  act on a project; an unknown name has no machine to act on. `POST /register`
  uses the same axis: an unknown project is `404`, not a permission error.
- **Unknown project, introspection** (`/introspect`) → `200` with
  `is_known_to_daemon: false`. Introspection is a *query about whether the
  daemon knows a project*; "no" is a valid answer, not an error.
- **Resolution** (`/resolve/{name}`) → a miss is **always `404`, never `503`**:
  "not resolvable right now" is an answer about the name, not a daemon fault.
- **Enablement, store unavailable** (the `/state` family) → **`503` with
  `degraded: true`**, the one deliberate `503` on the surface. Enablement
  reads fail closed: when the store could not open, "no answer" is served as
  a named daemon fault, never invented as a state. This does not soften the
  resolution rule above — `/resolve` misses stay `404`.

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

Status: always `200`. This is the **liveness** target in the kube probes -- it says
the process is up and config loaded. For "can it actually do its job," see `/readyz`.

## GET /readyz

Readiness probe. Distinct from `/health`: liveness asks whether the process is up
(restart it if not); readiness asks whether delightd can actually do its work right
now (stop routing to it if not, but do not restart -- a failed mount or an
unreachable apiserver is not fixed by a restart). Green only when every check passes.

```json
{ "ready": true, "checks": [
  { "name": "roots_readable", "ok": true },
  { "name": "kubectl_reachable", "ok": true }
] }
```

| Check | Meaning |
|-------|---------|
| `roots_readable` | the operating roots (monitor / daemon / config) exist and are readable. In a container these are the volume mounts, so a mount that failed to attach shows up red here rather than as silent misbehaviour downstream. |
| `kubectl_reachable` | kubectl reaches the apiserver (`get --raw=/readyz`, 3s timeout). delightd operates the cluster, so a delightd that cannot reach kubectl is not ready. |

Status: `200` when ready; `503` when any check fails, with the failing check's `ok`
set false and its `error` naming why -- never a bare "not ready". One HTTP answer
serves a human (via the wrapper's `status`), hm over the wire, and any service.

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

## GET /projects

The authoritative **roster**: every managed project with the fields fleet needs
to act on it, returned under `projects`. This is the seam where delightd owns the
roster and fleet reads it instead of parsing `WorkstationConfig.yaml` (see
[fleet-and-delightd.md](fleet-and-delightd.md)).

```json
{
  "status": "ok",
  "projects": [
    {
      "name": "paling",
      "path": "~/work/paling",
      "essential": false,
      "deploy": { "kind": "launchd", "command": ["uv", "run", "paling", "launchagent", "install"] },
      "remote_url": "git@github.com:janearc/paling.git"
    }
  ]
}
```

| Field | Meaning |
|-------|---------|
| `name` | The project's name (its key in the registry). |
| `path` | The project's working-tree path. |
| `essential` | Tier: `true` for the set bootstrap converges on a cold machine, `false` for on-demand workloads. fleet's tier-0 classification reads this. |
| `deploy.kind` | How fleet rolls the project: `compose`, `kube`, or `launchd`. Empty for projects that ship no service (CLI tools, libraries). |
| `deploy.deployment` | The kube Deployment name, when `kind: kube`. |
| `deploy.command` | The command fleet runs, when `kind: launchd`. |
| `remote_url` | The tracking remote, read per-request (cheap: repo config only, no worktree walk). Omitted when no remote resolves. |

Status: always `200`.

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

## GET /state

The enablement home: every managed project's effective enable/disable state,
roster-driven — a project with no stored record appears as
`disabled`/`recorded: false` rather than being omitted. This is the
fail-closed read: absence is rendered as disabled, never as a hole.

```json
{
  "projects": [
    { "project": "delightd", "state": "enabled", "recorded": true,
      "actor": "max", "changed_at": "2026-07-05T07:20:38Z" },
    { "project": "paling", "state": "disabled", "recorded": false }
  ]
}
```

| Field | Meaning |
|-------|---------|
| `project` | roster name |
| `state` | `enabled` \| `disabled` — the effective answer after doctrine |
| `recorded` | whether a stored record exists (`false` means the disabled answer is the absent-record default) |
| `reason` | why, when recorded; required on every disable |
| `actor` | who ruled, when recorded |
| `changed_at` | when the record last changed, when recorded |

Status: `200`; `503` `{"degraded": true, ...}` when the store is unavailable
(see the status-semantics list above).

## GET /state/{name}

One project's effective enablement, same rendering and doctrine as the list.

Status: `200` for a known project (absent record reads
`disabled`/`recorded: false`); `404` `{"error": "project not found"}` for a
name outside the roster; `503` degraded when the store is unavailable.

## PUT /state/{name}

The idempotent enablement write: the full desired state in the body, the
project in the path, last write wins. Roster-bound — state binds to the
canonical unit list, not free text.

```json
{ "state": "disabled", "reason": "flaky disk", "actor": "max" }
```

| Field | Meaning |
|-------|---------|
| `state` | `enabled` or `disabled`; anything else is `400` |
| `reason` | required when disabling — a disabled project with no recorded why is an operational lie |
| `actor` | who is ruling; required |

The response is the stored record as `GET /state/{name}` would render it.

Status: `200` on success; `400` on an unparseable body, an unknown state, or
a disable without a reason; `404` for a name outside the roster; `503`
degraded when the store is unavailable.

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
> [project taxonomy](architecture.md#6-taxonomy-what-is-a-project)
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

## POST /register

A frood joins the live registry (additive today: delightd does not yet require a
frood to register). The body is a protojson `registry.v1.RegisterRequest`; the
daemon runs five gates in order — roster membership, identity consistency, a
reported endpoint, contract verification against the schema registry
(fail-closed), and a live `/health` dial-back to the reported endpoint — and only
then records the registration under a lease.

```json
{
  "identity": { "serviceName": "magpie", "project": "magpie", "version": "0.1.0" },
  "endpoint": { "scheme": "http", "address": "magpie:8092" },
  "leaseTtlSeconds": 90
}
```

| Status | Condition |
|--------|-----------|
| `200` | accepted; protojson `registry.v1.RegisterResponse` (identity, confirmed endpoint, lease TTL) |
| `400` | unreadable or malformed `RegisterRequest` |
| `404` | unknown project (the roster convention: no project, nothing to act on) |
| `409` | endpoint already held by a different project |
| `422` | inconsistent identity, missing endpoint, unknown subject, or the `/health` guarantee failed |
| `503` | the schema registry could not be reached to verify a subject (fail-closed) |

Every decline also emits a never-silent `registry.v1.NotRegistered` event on a
detached goroutine — the HTTP response never waits on the broker.

## GET /registrations

The live registered set (`registry.v1.RegistrationSet`, each entry protojson) —
who has actually joined, alongside the static roster `GET /projects` declares.

```json
{
  "status": "ok",
  "registrations": [
    {
      "project": "magpie",
      "identity": { "serviceName": "magpie", "project": "magpie", "version": "0.1.0" },
      "contracts": { "emits": [ { "subject": "observability.v1.ServiceHealthHeartbeat" } ] },
      "endpoint": { "scheme": "http", "address": "magpie:8092" },
      "registeredAt": "2026-07-02T08:00:00Z",
      "leaseExpiresAt": "2026-07-02T08:01:30Z"
    }
  ]
}
```

Status: always `200`; an empty `registrations` array means nothing is currently
registered (a nil registry, as in tests, serves the empty set).

## GET /resolve/{name}

Narrow, widget-facing resolution: the scheme + address to reach one project, as a
protojson `resolve.v1.ResolvedService`. A resolution miss is **always `404`,
never `503`** — "not resolvable right now" is an answer about the name, not a
daemon fault.

```json
{ "scheme": "http", "address": "127.0.0.1:8092" }
```

| Status | Condition |
|--------|-----------|
| `200` | resolvable; protojson `resolve.v1.ResolvedService` |
| `404` | `{"error": "service not resolvable"}` — unknown name, or no live registration to resolve to |

## GET /services

The composed roster as an entity-query list: every managed project with its
facets (git / backup / reachable / endpoint) as fields. An optional `?type=`
filter narrows by service type; an unrecognized type is a `400` rather than a
silent empty list.

| Status | Condition |
|--------|-----------|
| `200` | `{"status": "ok", ...}` list envelope of composed entries |
| `400` | `{"error": "unknown service type: <type>"}` |

## GET /services/{name}

One composed roster entry, facets as fields. An entity delightd does not manage
is a `404` — same axis as the control routes: no project, nothing to compose.

| Status | Condition |
|--------|-----------|
| `200` | the composed entry for `{name}` |
| `404` | not a managed project |

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
