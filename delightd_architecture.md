# Architectural Invariants: delightd

## 1. Overview
`delightd` serves dual roles:
1. **Continuous Agentic Checkpointing (CAC)**: A state machine that mitigates destructive AI failures by orchestrating manifest-driven `.tgz` archiving based on filesystem churn metrics.
2. **Active Control Plane**: An active service mesh monitor that dynamically discovers rogue or standalone local LLMs (like `llama-server` or `ollama`), automatically registers them with `traefik` for routing, and exposes telemetry to dashboards. It is externally controlled via the `fleet-svc` tooling.

## 2. Infrastructure
- **Implementation**: Statically compiled Go binary (~15MB).
- **Execution Model**: Deterministic polling loop with HTTP/REST multiplexer for telemetry (`/metrics`, `/health`, `/mcp`).

## 3. State Management (Git Oracle)
The daemon relies strictly on Git to determine working tree state, avoiding expensive polling heuristics (e.g. `inotify`/`fsevents`).
- Polls `git status --porcelain` at defined intervals.
- Yields to sleep during clean states.
- Triggers the backup state machine on dirty modifications.

## 4. Archival Pipeline
Archival execution is strict and deterministic.
1. **Resolution**: `find` builds an absolute manifest of tracking paths, respecting standard exclusions (`.git`, object files, large caches).
2. **Compression**: The manifest is compressed via `tar` into a gzip format (`.tgz`).
3. **Retention**: Output is written to a timestamped file. Storage footprints are constrained by `max_archives`, actively rolling off legacy state files to enforce strict disk hygiene.

## 5. Configuration Topology
```yaml
system:
  root: "/work/delightd"
  config_root: "/etc/delightd"
  daemon:
    control_port: 8088

projects:
  - name: "odysseus"
    path: "/work/odysseus"
    backup:
      check_interval: "15m"
      rotation:
        max_archives: 48
```

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

Wire shape follows the taxonomy: git state is returned as an **element of a project**, e.g. `{"name": "odysseus", "git": {"branch": "main", "dirty": false, …}}` — not as a free-standing record.
