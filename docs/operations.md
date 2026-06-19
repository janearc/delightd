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
  monitor_root: "~/work"                 # tree delightd monitors (managed projects)
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
    path: "~/work/paling"
    backup:
      check_interval: "15m"              # Go duration string
      rotation:
        max_archives: 48                 # <= 0 means keep everything
      exclude:
        - "models"                       # name-matched at any depth
```

| Key | Type | Meaning |
|-----|------|---------|
| `system.monitor_root` | path | the tree delightd monitors (parent of the managed projects' git trees); canonical `~/work` |
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
> to `8088` (`config.DefaultControlPort`); compose and kube agree. Configure
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
| `DELIGHT_MONITOR_ROOT` | `system.monitor_root` | `~/work` |
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

## Kubernetes deployment

Live manifests are under **`kube/`** (`deployment.yaml`, `service.yaml`,
`kustomization.yaml`), namespace **`fleet`**. Validate without a cluster:

```bash
kubectl apply --dry-run=client -k kube/
```

Do not `kubectl apply` to the cluster and do not pull/import images — that is the
primary agent's gated step.

### Mounts (the storage contract)

| Mount | Path in container | Mode | Why |
|-------|-------------------|------|-----|
| host `~/work` | `/work` | **read-only** | git-state source; delightd reads project trees, never writes them |
| host `~/etc/delightd` | `/etc/delightd` | read-only | `delight.yaml` + registry |
| host `~/var` | `/var` | read-write | the one write surface: backups, `/var/bin`, traefik dynamic |

`/work` is **read-only by contract** — delightd observes git state, it does not
own the working trees. The roots map onto the mounts: `DELIGHT_MONITOR_ROOT=/work`
(read-only git-state source), `DELIGHT_DAEMON_ROOT=/var` and
`DELIGHT_BACKUPS_ROOT=/var/backups` (the `/var` write surface),
`DELIGHT_CONFIG_ROOT=/etc/delightd`. Backups land on `/var`, never under the
read-only `/work`. (An earlier compose set a single `DELIGHT_SYSTEM_ROOT` that
both overloaded the path and, on `/work/backups`, needed `/work` writable; the
split roots remove both problems.)

### Other deployment facts

- **Port.** Container port `control` = `8088`; the `Service` (ClusterIP) exposes
  the same. In-cluster consumers (fleet-svc) address delightd by Service name;
  edge traffic reaches it through traefik, not a NodePort.
- **Probes.** Readiness and liveness both `httpGet /health` on the `control`
  port.
- **User.** Runs as the host engineer's UID/GID (`1000`) so archives and shims it
  writes under `/var` stay host-owned, never root.
- **RBAC.** None. delightd does not call the Kubernetes API (no client-go); do
  not grant it a ServiceAccount role.
- **Image.** Locally built `delightd-delightd:latest`, imported into k3d by the
  primary agent (`k3d image import`). It is not a pinned version tag yet — flag
  for a real tag before any fleet rollout.

## Build

The Taskfile is the entry point (`buf` required for proto generation):

```bash
task generate    # buf generate -> gen/ (gitignored, never committed)
task build       # generate, then go build -o bin/delightd ./cmd/delightd
task test        # generate, then go test ./...
task sync-proto  # re-vendor delight.v1 from kafka-svc, then run generate
```

## Removed and stale

| Removed | Replacement |
|---------|-------------|
| `k8s/delightd.yaml` (namespace `dev-fleet`, old port, `--dry-run`) | `kube/` manifests (namespace `fleet`, `:8088`, live) |
| `envoy.yaml` (abandoned proxy path) | traefik is the single edge; no Envoy |

Both were deleted in this docs rewrite. The Envoy/"dual proxy profile"
deployment model in the old `INSTALL.md` no longer exists — there is one ingress
(traefik), not a choice of proxies.
