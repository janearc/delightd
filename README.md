# delightd

`delightd` is the daemon responsible for fleet checkpointing and interface aggregation. It evaluates git repositories for churn, manages local `.tgz` snapshot archives, and exposes binary paths to the host environment.

## Architecture

The daemon is integrated directly into the service mesh. It exposes an HTTP control port for metrics and Model Context Protocol (MCP) telemetry.

It is compiled as a static 15MB Go binary. It intentionally omits network routing SDKs, delegating ingress to external infrastructure profiles (Traefik or Envoy).

See `INSTALL.md` for deployment constraints. See `DEVELOPERS.md` for project integration invariants.

## Interface Exports

The daemon scans managed projects in `~/work` and enforces executable paths into the host `$PATH` at `~/var/bin`.

### Static Execution

Executables located in `~/work/<project>/bin/` are symlinked into `~/var/bin`.

### Docker Shims

To bridge the gap between host-level orchestration and strict containerized isolation, the daemon dynamically writes transparent shell wrapper scripts to `~/var/runtime/delightd/exports/` and symlinks them. This allows the host to execute standard shell commands that transparently proxy into Docker boundaries. Execution modes are defined in `~/etc/delight-registry.yaml`.

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

## Execution

```bash
docker compose up -d
```
