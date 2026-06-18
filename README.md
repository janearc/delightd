# 😋 delightd

`delightd` is the daemon responsible for fleet checkpointing, interface aggregation, and active control plane duties. It evaluates git repositories for churn, manages local `.tgz` snapshot archives, dynamically discovers local LLMs, registers them to `traefik`, and is controlled externally via `fleet-svc`.

## Architecture

The daemon is integrated directly into the service mesh. It exposes an HTTP control port for metrics, LLM telemetry, and Model Context Protocol (MCP).

It is compiled as a static 15MB Go binary. It generates dynamic routing configurations for Traefik to pull newly discovered local services (like `llama-server`) into the mesh automatically.

See `INSTALL.md` for deployment constraints. See `DEVELOPERS.md` for project integration invariants.

## Interface Exports

The daemon scans managed projects in `~/work` and enforces executable paths into the host `$PATH` at `~/var/bin`.

### Static Execution

Executables located in `~/work/<project>/bin/` are symlinked into `~/var/bin`.

### Docker Shims (Bash Fragments)

To bridge the gap between host-level orchestration and strict containerized isolation, the daemon dynamically writes transparent shell wrapper scripts to `~/var/runtime/delightd/exports/<project>/<bin>.sh` and symlinks them into `~/var/bin`. This allows the host to execute standard shell commands that transparently proxy into Docker boundaries. Execution modes are defined in `~/etc/delight-registry.yaml`.

A single one of these generated wrapper scripts is what the control API calls a **bash fragment**. The term is literal and narrow: it is a `<bin>.sh` shell script the daemon generated for a project. Nothing more sinister. Introspection reports `has_bash_fragment: true` for a service when at least one such script exists on disk under that project's export directory — that on-disk presence is the source of truth.

**docker-run (Ephemeral):**
```bash
#!/usr/bin/env bash
exec docker run --rm -i -v "$(pwd):/workspace" -w /workspace <image> "$@"
```

**docker-exec (Persistent):**
```bash
#!/usr/bin/env bash
exec docker exec -i <container> <command> "$@"
```

The daemon provides idempotent cleanup. Unlinked exports are archived to `~/var/archive/delightd/exports/<timestamp>/`.

## Introspection

`GET /projects/{name}/introspect` returns the daemon's view of a single service:

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
| `is_known_to_daemon` | The service is present in the daemon's project configuration. |
| `is_actively_backing_up` | The service's backup state machine is currently in `backing_up`. |
| `has_bash_fragment` | At least one generated docker shim (see [Docker Shims](#docker-shims-bash-fragments)) exists for the service. |

An unknown service returns `200` with `is_known_to_daemon: false`, not `404`: a query for a service the daemon has never heard of is a valid answer worth tracking as a signal, not an error.

## Git State

`GET /git` returns live git state for every managed project; `GET /projects/{name}/git` returns it for one:

```json
{
  "name": "paling",
  "branch": "main",
  "dirty": false,
  "unpushed": 0,
  "has_upstream": true,
  "remote_url": "git@github.com:janearc/paling.git"
}
```

| Field | Meaning |
|-------|---------|
| `branch` | The currently checked-out branch (empty in a detached HEAD). |
| `dirty` | The working tree has uncommitted changes (tracked or untracked). |
| `unpushed` | Commits reachable from `HEAD` that are not on `origin/<branch>`. |
| `has_upstream` | An `origin/<branch>` tracking ref exists. When `false`, the branch has never been pushed and every local commit counts as `unpushed`. |
| `remote_url` | The `origin` remote URL, if configured. |
| `error` | Present only when the repo could not be read (e.g. not a git checkout); other fields hold zero values. A failure on one repo never aborts the `/git` sweep. |

State is computed **live, per request** with `go-git` — not served from a cache. This is deliberate: `fleet-svc` gates destructive host-migration on the dirty/unpushed answer, so a stale "clean" reading could greenlight a teardown over uncommitted work. Field names are aligned with the forthcoming `delight.v1.RepoGitState` contract so the surface graduates to Protobuf over Kafka with the daemon's other events.

## Execution

```bash
docker compose up -d
```
