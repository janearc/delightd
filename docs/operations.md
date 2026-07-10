# delightd — operations and configuration

This document covers running delightd: the `delight.yaml` schema, the `DELIGHT_*`
environment surface, the kube deployment, and the build. It replaces the old
`INSTALL.md`. For what the daemon *does*, see
[architecture.md](architecture.md).

## Configuration sources, in order

delightd loads config with viper (`config.Load`):

1. `delight.yaml` from `$HOME/etc/delightd`, then the current directory.
2. `DELIGHT_*` environment overrides (always applied; override file values).

A missing config file is not fatal — the daemon logs a warning and runs on env
vars and defaults.

## delight.yaml schema

```yaml
system:
  # monitor_root deliberately unset -- default $DELIGHT_INSTALL_ROOT, else ~/mesh/prod
  daemon_root: "~/var"                   # delightd's own runtime/state tree
  # backups_root defaults to ${daemon_root}/backups; set to relocate backups
  # backups_root: "~/var/backups"
  config_root: "~/etc"                   # config + registry resolution dir
  daemon:
    control_port: 8088                   # canonical control port
    pid_file: "~/var/run/delightd.pid"
  agent_skills:
    enabled: true
    expose_via: ["mcp", "cli"]           # any subset of mcp, cli
  llm_discovery:
    providers:
      - name: "ollama-local"
        type: "ollama"                   # ollama | llama_cpp | openai | apfel
        url: "http://localhost:11434"
  kafka:
    brokers: ["kafka:9092"]              # empty => event publishing disabled
    schema_registry_url: "http://schema-registry:8081"
    topic: "delight.events"

projects:
  - name: "paling"
    path: "~/mesh/prod/paling"
    backup:
      check_interval: "15m"              # Go duration string
      rotation:
        max_archives: 48                 # <= 0 means keep everything
      exclude:
        - "models"                       # name-matched at any depth
```

| Key | Type | Meaning |
|-----|------|---------|
| `system.monitor_root` | path | the tree delightd monitors (parent of the managed projects' git trees); default `$DELIGHT_INSTALL_ROOT`, else `~/mesh/prod` |
| `system.daemon_root` | path | delightd's own runtime/state tree; canonical `~/var` |
| `system.backups_root` | path | the backup destination directory itself (no `/backups` appended); defaults to `${daemon_root}/backups`, set to relocate backups independently |
| `system.config_root` | path | config + registry resolution dir; canonical `~/etc` |
| `system.daemon.control_port` | int | HTTP control port; canonical `8088` |
| `system.daemon.pid_file` | path | pid file location |
| `system.agent_skills.enabled` | bool | enable the skill aggregator + CLI/MCP exposure |
| `system.agent_skills.expose_via` | `[]string` | `mcp` registers `POST /mcp`; `cli` generates `~/var/bin/delight` |
| `system.llm_discovery.providers[]` | list | local LLM endpoints to probe (`name`, `type`, `url`) |
| `system.kafka.brokers` | `[]string` | empty disables the publisher entirely |
| `system.kafka.schema_registry_url` | url | Confluent Schema Registry REST base |
| `system.kafka.topic` | string | event topic (`delight.events`) |
| `projects[].name` | string | the project's canonical name |
| `projects[].path` | path | working-tree path (`~` expanded) |
| `projects[].backup.check_interval` | duration | git-oracle poll interval |
| `projects[].backup.rotation.max_archives` | int | retained `.tgz` count; `<= 0` keeps all |
| `projects[].backup.exclude` | `[]string` | extra paths/names kept out of the tar |

> Port note. The committed `delight.yaml` and `main.go`'s fallback both resolve
> to `8088` (`config.DefaultControlPort`); the kube manifest agrees. Configure
> `8088`.

## Environment variables

Two override mechanisms exist, and they are independent.

**1. viper config overrides** — most `delight.yaml` keys map by prefixing
`DELIGHT_` and replacing `.` with `_`. The four roots are the exception: each is
bound explicitly to a short env name (without the `SYSTEM_` segment), so a
relocated layout can be expressed cleanly and is read even when no config file is
present:

| Variable | Overrides | Default |
|----------|-----------|---------|
| `DELIGHT_MONITOR_ROOT` | `system.monitor_root` | `$DELIGHT_INSTALL_ROOT`, else `~/mesh/prod` |
| `DELIGHT_INSTALL_ROOT` | seeds `system.monitor_root`'s default | `~/mesh/prod` -- relocates the whole host fleet layout with one variable |
| `DELIGHT_DAEMON_ROOT` | `system.daemon_root` | `~/var` |
| `DELIGHT_BACKUPS_ROOT` | `system.backups_root` | `${DELIGHT_DAEMON_ROOT}/backups` |
| `DELIGHT_CONFIG_ROOT` | `system.config_root` | `~/etc` |
| `DELIGHT_SYSTEM_DAEMON_CONTROL_PORT` | `system.daemon.control_port` | `8088` |
| `DELIGHT_SYSTEM_KAFKA_BROKERS` | `system.kafka.brokers` | — |
| `DELIGHT_SYSTEM_KAFKA_SCHEMA_REGISTRY_URL` | `system.kafka.schema_registry_url` | — |
| `DELIGHT_SYSTEM_KAFKA_TOPIC` | `system.kafka.topic` | — |

`BACKUPS_ROOT` derives from `DAEMON_ROOT` when unset; setting it explicitly
overrides the derivation (it is the literal destination, never a parent the
daemon appends `/backups` to).

**2. exports-engine paths** — read directly by `pkg/exports`, not through viper.
These govern where generated wrappers, shims, and the registry live:

| Variable | Default | Meaning |
|----------|---------|---------|
| `DELIGHT_EXPORTS_REGISTRY` | `~/etc/delight-registry.yaml` | docker-tool registry the exports engine reads |
| `DELIGHT_EXPORTS_BIN` | `~/var/bin` | where the `delight` CLI and symlinks are written |
| `DELIGHT_EXPORTS_STATE` | `~/var/runtime/delightd/exports` | generated shim scripts |
| `DELIGHT_EXPORTS_ARCHIVE` | `~/var/archive/delightd/exports` | archived (unlinked) exports |

> Registry path note. The code default is `~/etc/delight-registry.yaml`; the kube
> deployment sets `/etc/delightd/delight-registry.yaml`. They differ — set
> `DELIGHT_EXPORTS_REGISTRY` to pin the path you intend.

## Flags

| Flag | Effect |
|------|--------|
| `--dry-run` | walk manifests and exports without writing any archive, symlink, or shim |
| `--immediate` | evaluate every project once on startup instead of waiting for the first interval tick |
| `--help` | print usage and exit 0; no side effects (cobra built-in; the install smoke check rests on it) |

## Kubernetes deployment

delightd's environment is declared, not hand-assembled. Every furnished piece
runs from a manifest under **`kube/`**, one directory per piece (surrealdb,
kafka, the rest of the furniture), namespace **`fleet`**. A top-level
`kube/kustomization.yaml` aggregates them. delightd itself is deliberately
**not** a piece: it is the operator, a host-level container (see Build and the
wrapper), never a pod, never supervised by the fleet it operates. There are no
in-cluster delightd manifests to render or deploy.

Converging a cluster onto what these manifests declare is delightd's own job,
through the `furnish` command: `delightd furnish list` names the declared
pieces (the aggregator's resources list is the declaration — a directory not
listed there does not exist to furnish), `delightd furnish up <piece>`
converges one piece onto its manifests, `delightd furnish down <piece>`
removes it (absent is success), and `delightd furnish health [piece]` walks
the ladder and exits non-zero when any object is RED (observed unhealthy, or
declared-but-absent) or INDETERMINATE (could not be read at all — transport,
RBAC, or timeout). The two are labelled distinctly so you can tell a broken
piece from an unreachable one, and one unreadable object does not blind the
rest of the piece. Output is JSON. You drive delightd; you do not hand-run
`kubectl` against the cluster — and neither does furnish: it speaks to the
apiserver through client-go, so no `kubectl` binary is required.

To check that the manifests build — no cluster, no API server contact — render
them locally with kustomize:

```bash
kubectl kustomize kube/            # the whole environment
kubectl kustomize kube/surrealdb/  # one piece in isolation
```

When the machine itself has just come back — a reboot, an OS upgrade, a power
event — start with [recovery.md](recovery.md) instead: it trues the whole
stack (toolchain, colima, k3d, the daemon, furnishings) in order, and none of
the commands above are useful until it has run.

### Mounts (the storage contract)

| Mount | Path in container | Mode | Why |
|-------|-------------------|------|-----|
| host `$DELIGHT_MONITOR_ROOT_HOST` (e.g. `~/mesh/prod`) | `/work` | **read-only** | git-state source; delightd reads project trees, never writes them |
| host `~/var` | `/var` | read-write | the one write surface: backups, `/var/bin`, traefik dynamic |
| creds dir (wrapper-managed, under `$HOME`) | `/run/delightd` | **read-only** | credentials delivered at runtime: the credential-less `kubeconfig`, the k8s `token` it points `tokenFile` at, and `control-token` (the control-port bearer). None of these are baked into the image or set as env/argv; the wrapper's `resolve_creds` writes them atomically and this is the only mount that carries a secret. |

`delight.yaml`, the registry, and the `kube/` manifest tree are **not** a mount
— they are baked into the image at build time (see Build below) so a running
orchestrator is pinned to its image, not to a mutable host path. `/work` is
**read-only by contract** — delightd observes git state, it does not own the
working trees. The roots map onto the mounts: `DELIGHT_MONITOR_ROOT=/work`
(read-only git-state source), `DELIGHT_DAEMON_ROOT=/var` and
`DELIGHT_BACKUPS_ROOT=/var/backups` (the `/var` write surface),
`DELIGHT_CONFIG_ROOT=/etc/delightd` (the baked config tree, not a mount).
Backups land on `/var`, never under the read-only `/work`.

### Other deployment facts

- **Port.** Container port `control` = `8088`; `docker-compose.yml` publishes
  it as `127.0.0.1:8088` on the host, and the container is reachable on its
  container networks (`dev-fleet`, `ring0`) and through the traefik edge route.
  There is no Kubernetes Service for delightd -- it is not a pod.
- **Probes.** The image carries a Dockerfile `HEALTHCHECK` running
  `delightd healthcheck` (an exec-form self-probe of `GET /readyz`; the
  scratch image has no shell or curl). Green means the roots are mounted and
  the apiserver is reachable, not merely that a process exists.
- **User.** The container runs as root (no `user:` override in
  `docker-compose.yml`). On colima's virtiofs share, a non-root container UID
  does not map onto the host engineer's ownership of the `/var` write mount,
  so writes fail; root is the reliable execution user. This is
  container-namespaced root, not host root, and it is compensated: the
  container runs `read_only: true` (locks the image's own layers and
  anything not named under `volumes:`), `cap_drop: [ALL]` (a Go daemon
  speaking HTTP + the k8s API needs no Linux capability), and
  `security_opt: [no-new-privileges:true]`. See `docker-compose.yml` for the
  per-write-path accounting (every path the daemon writes resolves under the
  `/var` mount) that justifies `read_only` costing nothing functional.
- **API access.** delightd calls the Kubernetes API via client-go — `furnish`
  converges the meubilair pieces with kustomize + server-side apply and reads their
  health (see `docs/kubernetes-access.md`). It needs a kubeconfig whose identity can
  read and apply the fleet's objects; as the host-level operator it uses the operator's
  kubeconfig, not an in-cluster ServiceAccount.

## Build

The Taskfile is the entry point (`buf` required for proto generation):

```bash
task generate          # buf generate -> gen/ (gitignored, never committed)
task build             # generate, then go build -o bin/delightd ./cmd/delightd
task test              # generate, then go test ./...
task sync-proto        # re-vendor delight.v1 from kafka-svc, then run generate
task e2e-registration  # prove the magpie->delightd registration seam end to end
                       # (local-first: needs magpie checked out under the fleet root
                       # -- e.g. ~/mesh/prod/magpie -- and uv on PATH)
```

## Install from a checkout

delightd runs as a host-level container now, not an on-disk Go binary.
`scripts/install.sh` brings a checkout up to date (or clones it if absent),
builds the container image, and symlinks the `delightd` wrapper
(`scripts/delightd` — the SRE front door: `start`/`stop`/`logs`/`status`,
everything else forwarded to the control port) onto `$HOME/var/bin/delightd`.
It is idempotent and takes no hand steps — no prompts, no sudo, and nothing
over the wire but git and the image build. Override the checkout path with
`DELIGHTD_SRC` (default `$DELIGHT_INSTALL_ROOT/delightd`, which itself
defaults to `$HOME/mesh/prod/delightd`).

```bash
scripts/install.sh
```

It requires **docker** and **git** on the host — nothing else. The image
builds its own Go/buf toolchain inside the builder stage (see the
[Dockerfile](../Dockerfile)), so there is no separate host toolchain to
install or keep in sync.

On a clean checkout there is no `.env` yet. `scripts/install.sh` bootstraps one from
`.env.example`, prints what to fill in for this machine (the mount roots, the
1Password vault reference, cluster access), and **stops** — `.env`'s
placeholder paths are wrong for any real machine, and the install refuses to
build against them rather than silently mounting garbage on a later
`delightd start`. Fill in `.env` and rerun `scripts/install.sh` to complete the build.

## Removed and stale

| Removed | Replacement |
|---------|-------------|
| `k8s/delightd.yaml` (namespace `dev-fleet`, old port, `--dry-run`) | the host-level operator container behind the wrapper (`:8088`, live); `kube/` holds only the furnished pieces, never delightd |
| `envoy.yaml` (abandoned proxy path) | traefik is the single edge; no Envoy |

Both were deleted in this docs rewrite. The Envoy/"dual proxy profile"
deployment model in the old `INSTALL.md` no longer exists — there is one ingress
(traefik), not a choice of proxies.
