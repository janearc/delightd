# delightd — architecture

delightd is the fleet's control plane: a single Go binary with one HTTP control
port. The rest of the fleet reads project state from it and fails closed when it
is absent (see [docs/availability.md](docs/availability.md)). This document
describes the components, the git oracle, the archival pipeline, and the
taxonomy the API shapes are built on.

## 1. Component map

delightd is one process. Inside it, a set of independent packages each own a
slice of the daemon's responsibility; `cmd/delightd/main.go` is wiring and the
control loop, nothing more. Handlers live in `pkg/httpapi` so they are
unit-testable against explicit dependencies rather than as closures in `main`.

| Component | Package | Responsibility |
|-----------|---------|----------------|
| Config | `config` | loads `delight.yaml` via viper; `DELIGHT_*` env overrides |
| Control-port HTTP | `pkg/httpapi` | the route table; constructs handlers against injected deps |
| Backup state machine | `pkg/state` | per-project `fallow → monitoring → backing_up → fallow`, with an `error` backoff state |
| Git oracle (churn) | `pkg/watcher` | per-interval go-git working-tree check that drives the state machine |
| Git state (reporting) | `pkg/gitstate` | live, per-request git state for the `/git` surface |
| Backup pipeline | `pkg/backup` | builds and writes the `.tgz`; enforces rotation; applies excludes |
| Event publisher | `pkg/events` | emits `delight.v1.BackupEvent` to Kafka (best-effort) |
| Introspection | `pkg/introspect` | composes state-machine status with the exports view |
| LLM discovery | `pkg/discovery` | probes configured/standard local LLM endpoints |
| traefik routing | `pkg/traefik` | writes dynamic routes for discovered LLMs |
| Exports engine | `pkg/exports` | symlinks/wrappers into `~/var/bin`; generates docker shims |
| Skill aggregator | `pkg/skills` | scans `mcp.json`, serves MCP, generates the `delight` CLI |
| Metrics | `pkg/metrics` | prometheus exposition on `/metrics` |

The control loop in `main` runs three periodic goroutines plus one per project:

| Loop | Cadence | Action |
|------|---------|--------|
| Export + skill sync | 5 min | re-scan projects; regenerate exports and the `delight` CLI |
| LLM discovery | 30 s | probe local LLMs; sync traefik routes |
| Per-project poll | `check_interval` | run the git oracle while the project is `fallow`/`monitoring` |
| Per-project eval | 2 s | execute the backup when the machine is `backing_up`; retry after error backoff |

`machines` (the per-project state map) is built once at startup and only read
afterwards, so handlers read it without a lock; each `Machine` guards its own
state internally.

## 2. Runtime

| Property | Value |
|----------|-------|
| Implementation | statically-linked Go binary (no cgo) |
| Control port | `:8088` (canonical; see note below) |
| Logging | structured JSON to stdout (`slog`) |
| Flags | `--dry-run` (walk manifests, write nothing), `--immediate` (evaluate on startup without waiting for the first tick) |
| Shutdown | SIGINT/SIGTERM → 5 s graceful HTTP shutdown |

> Control-port note. The canonical port is `:8088`. The current `main.go`
> defaults to `8080` when `control_port` is unset, and the committed
> `delight.yaml` still sets `8080`; a separate config-fix PR corrects both. This
> document states `:8088` as the contract regardless.

## 3. The git oracle (churn detection)

The daemon decides *when* to checkpoint a project by reading its git working
tree, not by watching the filesystem (`inotify`/`fsevents`). This is the "git
oracle": a dirty tree is the signal that there is work worth checkpointing.

The check is performed **with go-git in-process** (`pkg/watcher`): it opens the
repository, reads the worktree status, and reports churn when the tree is not
clean. It does **not** shell out to `git status --porcelain`. Only projects in
`fallow` or `monitoring` are polled; while a backup is running or in error
backoff there is nothing to react to, so the poll is skipped.

The backup state machine (`pkg/state`):

```text
fallow ──churn──▶ monitoring ──churn/trigger──▶ backing_up ──success──▶ fallow
                                                     │
                                                   fail
                                                     ▼
                                                  error ──backoff expires──▶ backing_up
                                                     │
                                                 clear (reset)
                                                     ▼
                                                  fallow
```

`GET /projects/{name}/state` returns the machine's diagnostics
(`state`, `error_count`, `last_activity`, `next_retry`).

## 4. The archival pipeline

A checkpoint (`pkg/backup`) is deterministic and walks the project tree once,
through a single skip predicate shared by the dry-run manifest and the real tar
so they cannot disagree about what is included.

1. **Manifest** — walk the project directory, applying the built-in skips and the
   project's configured `exclude` list.
2. **Compress** — stream surviving regular files into a gzipped tar (`.tgz`).
3. **Write** — to `<backupRoot>/<project>/<project>-<timestamp>.tgz`.
4. **Rotate** — keep at most `max_archives` per project, deleting the oldest;
   `max_archives <= 0` keeps everything.

The hard invariant: **delightd only ever rotates its own `.tgz` archives. It
never deletes from model, weight, or cache directories.** Those are excluded
from the backup by name, not deleted. The full pipeline, the name-aware exclude
semantics, and the canonical `~/var/backups` path are in
[docs/backups.md](docs/backups.md).

After each checkpoint attempt the daemon emits a best-effort
`delight.v1.BackupEvent`; a Kafka or Schema-Registry outage never blocks or
fails the backup. See [docs/events.md](docs/events.md).

## 5. Configuration

delightd reads `delight.yaml` from `$HOME/etc/delightd` or the current
directory (viper), with `DELIGHT_*` environment overrides. A representative
config:

```yaml
system:
  monitor_root: "~/work"   # tree delightd monitors (managed projects)
  daemon_root: "~/var"     # delightd's own runtime/state tree
  # backups_root defaults to ${daemon_root}/backups -> ~/var/backups
  config_root: "~/etc"     # config + registry resolution dir
  daemon:
    control_port: 8088
    pid_file: "~/var/run/delightd.pid"
  agent_skills:
    enabled: true
    expose_via: ["mcp", "cli"]
  kafka:
    brokers: ["kafka:9092"]
    schema_registry_url: "http://schema-registry:8081"
    topic: "delight.events"

projects:
  - name: "paling"
    path: "~/work/paling"
    backup:
      check_interval: "15m"
      rotation:
        max_archives: 48
      exclude:
        - "models"         # name-matched at any depth; keeps weights out of the .tgz
```

The full `delight.yaml` schema and the complete `DELIGHT_*` env table are in
[docs/operations.md](docs/operations.md). `odysseus` is no longer in the fleet;
it appears in no current config.

## 6. Taxonomy: What Is a Project?

`project` is delightd's single atomic unit. Everything the daemon does — checkpoint, introspect, export, report git state — it does *to a project*. The term is deliberate, and the surrounding nouns are defined against it so the vocabulary stays consistent.

- **Project** — a named unit of work declared in `delight.yaml` (`name` + `path`). delightd **manages the project, not its repository**: the git working tree at `path` is the project's source of truth, which the daemon *observes* (see the Git Oracle) but does not own. A project is the noun; it is never called a "repo" in the API or code, because delightd does not manage repositories.
- **Kind** — what a project *is*. A project may be a **service** (it runs and serves), a **tool** (it is invoked), a **library**, and so on. "Service" is a *role a project plays*, not the unit itself — conflating the two is the inconsistency this taxonomy retires.
- **Deployment** — a *running instantiation* of a project (a container, a compose stack, a k8s workload). A project has zero, one, or many deployments at a given time. A project is not its deployment.
- **Capabilities** — what a project *exposes*: generated bash fragments (→ agent skills), exports.
- **GitState** — an *observed attribute* of a project's working tree: `branch`, `dirty`, `unpushed`, `has_upstream`, `remote_url`. Reported, never managed.

Relationships:

```text
project ── has ──────▶ GitState        (observed, one)
project ── is a ─────▶ Kind            (service | tool | library | …)
project ── has ──────▶ Deployment      (0..n, running instantiations)
project ── exposes ──▶ Capabilities    (skills, exports)
```

Wire shape follows the taxonomy: git state is returned as an **element of a project**, e.g. `{"name": "paling", "git": {"branch": "main", "dirty": false, …}}` — not as a free-standing record.

> Pending wire rename. The introspection type is still `ServiceBackupStatus`
> with fields `service_name` / `is_known_to_daemon`. It predates this taxonomy
> and is slated to rename to `project`. The current shape is documented in
> [docs/api.md](docs/api.md#get-projectsnameintrospect); treat the names as
> transitional, not final.
