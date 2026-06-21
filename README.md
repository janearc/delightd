# 😋 delightd

`delightd` is the fleet's control plane: a single statically-linked Go binary
with one HTTP control port that the rest of the fleet asks about project state —
git working-tree status, checkpoint (backup) status, project introspection,
discovered local LLMs, and the aggregated agent-tool surface. fleet-svc and the
other tooling are consumers of delightd, not the other way around.

## On the term "control plane"

Go already has control-plane libraries, and they solve a different problem than
this one. Envoy's [go-control-plane] and Google's gRPC xDS control plane are xDS
servers: they stream desired configuration out to data planes and own a snapshot
cache. Kubernetes [controller-runtime] reconciles declared state against
observed cluster state in a control loop.

delightd is neither. It distributes no configuration to data planes and runs no
reconcile loop. It answers, per request, what is true about a project right now —
git state, backup state, discovered LLMs — and emits an event for the one action
it owns, the checkpoint. Those libraries earn their complexity at a scale this
fleet is nowhere near: a large data-plane population that needs configuration
streamed to it and held in a snapshot cache. At this size, config distribution is
handled without them (traefik as the route registry; fleet-svc converges services
to the declared set), and delightd is the authority those parts read from.
go-control-plane is the answer for a much bigger fleet; if the fleet grows into
it, that is a new component alongside delightd, not a retrofit of it.

[go-control-plane]: https://github.com/envoyproxy/go-control-plane
[controller-runtime]: https://github.com/kubernetes-sigs/controller-runtime

## Availability contract (read this first)

delightd has no peer and no quorum. The consumers that depend on it — fleet-svc
in particular — **fail closed** when it is down. There is **no local fallback**:
fleet's `git status` over the fleet does not fall back to shelling out to git
itself; it returns an error if delightd does not answer.

The consequence: resilience is delightd's job — it must come up in any
condition — not the consumers'. Consumers do not hedge against its absence.

Concretely:

- A consumer that cannot reach delightd returns an error. It does not fall back
  to a locally computed answer.
- Any change to the consumer-facing surface (notably `GET /git`) requires a new
  delightd binary to be deployed before the dependent fleet commands work. There
  is no client-side reimplementation and no compatibility shim.
- The git-state surface is computed live, per request (see
  [git state](#git-state)), precisely so a consumer never acts on a stale answer.

See [docs/availability.md](docs/availability.md) for the full statement.

## What it does

| Responsibility | Surface | Notes |
|----------------|---------|-------|
| Checkpoint projects | backup pipeline, `POST /projects/{name}/backup` | rotating `.tgz` archives; never touches model/weight dirs |
| Report git state | `GET /git`, `GET /projects/{name}/git` | live, per-request, parallel whole-fleet sweep; delightd owns fleet git-state |
| Service introspection | `GET /projects/{name}/introspect` | known / backing-up / has-fragment; unknown is 200, not 404 |
| Discover local LLMs | `GET /discovery/llms` | probes configured/standard local LLM endpoints; registers routes with traefik |
| Aggregate agent tools | `POST /mcp`, generated `delight` CLI | scans each project's `mcp.json`, namespaces and serves them over MCP |
| Emit backup events | Kafka (best-effort) | first fleet Kafka producer; an outage never blocks a backup |

delightd owns fleet git-state (the `/git` surface); obs-svc owns the dashboard
that renders it.

## Taxonomy

delightd's canonical unit is the **project**, never "service", "deployment", or
"repo". delightd manages the *project*, not the repository at its path — git
state is an *observed attribute* of the project, not something delightd owns.
The full taxonomy (project / kind / deployment / capabilities / git-state) is in
[the architecture doc](docs/architecture.md#6-taxonomy-what-is-a-project) and is
load-bearing for the API shapes.

## Git state

Git state is an observed attribute of a [project](docs/architecture.md#6-taxonomy-what-is-a-project).
`GET /projects/{name}/git` returns one project; `GET /git` returns every managed
project under a `projects` array.

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
| `git.branch` | Currently checked-out branch (empty in a detached HEAD). |
| `git.dirty` | The working tree has uncommitted changes (tracked or untracked). |
| `git.unpushed` | Commits reachable from `HEAD` not on the branch's tracking ref. |
| `git.has_upstream` | A tracking ref exists. When `false`, the branch has never been pushed and every local commit counts as `unpushed`. |
| `git.remote_url` | The tracking remote's URL (resolved via the branch's upstream, then `origin`, then a sole remote). |
| `git.error` | Present only when a project could not be read. Other fields hold zero values. One project's failure never aborts the `/git` sweep. |

State is computed **live, per request** with `go-git` — not `git status
--porcelain`, and not served from a cache. This is deliberate: fleet-svc gates
destructive host-migration on the dirty/unpushed answer, so a stale "clean"
reading could greenlight a teardown over uncommitted work. Field names are
aligned with the forthcoming `delight.v1.GitState` contract so the surface
graduates to Protobuf over Kafka with the daemon's other events.

`GET /git` reads every project's tree **concurrently** with a per-project
deadline; the API reference covers the sweep behavior precisely
([docs/api.md](docs/api.md#get-git)).

## Introspection

`GET /projects/{name}/introspect` returns the daemon's view of a single project.
An unknown project returns `200` with `is_known_to_daemon: false`, **not** 404:
a query for a project the daemon has never heard of is a valid signal worth
recording, not an error. Field shape and the pending `service_name` → `project`
rename are in [docs/api.md](docs/api.md#get-projectsnameintrospect).

## Control port

The canonical control port is **`:8088`** — `delight.yaml`, `main.go`'s fallback
(`config.DefaultControlPort`), compose, and kube all agree.

## Documents

| Document | Contents |
|----------|----------|
| [docs/architecture.md](docs/architecture.md) | component map, the git oracle, archival pipeline, taxonomy |
| [docs/fleet-and-delightd.md](docs/fleet-and-delightd.md) | the control-plane consolidation: delightd owns the roster, fleet-svc wraps it; the seam and the direction |
| [docs/api.md](docs/api.md) | every control-port route: method, JSON shapes, status codes |
| [docs/availability.md](docs/availability.md) | fail-closed contract, deploy-before-use rule |
| [docs/events.md](docs/events.md) | the Kafka backup-event contract (wire format, SR, tradeoffs) |
| [docs/backups.md](docs/backups.md) | checkpoint pipeline, name-aware exclude, rotation, the never-touch-weights invariant |
| [docs/agent-interface.md](docs/agent-interface.md) | JSON-by-default, skill aggregator, `delight` CLI, registry + reload |
| [docs/operations.md](docs/operations.md) | `delight.yaml` schema, `DELIGHT_*` env table, kube deploy, build |
| [proto/README.md](proto/README.md) | how the `delight.v1` proto is vendored from kafka-svc |

## Build and run

```bash
task build      # regenerates proto bindings, builds bin/delightd
task test       # go test ./...
./bin/delightd  # reads delight.yaml from $HOME/etc/delightd or the cwd
```

Build and deployment detail (kube manifests, mounts, probes) is in
[docs/operations.md](docs/operations.md).
