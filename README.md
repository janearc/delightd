# Delightd: Autonomous Hyperscale Checkpoint Daemon

`delightd` is a lightweight, zero-dependency Go daemon designed to continuously poll massive Git repositories natively in-memory, evaluate churn, and compress local `.tgz` snapshot archives to prevent catastrophic data loss during autonomous burst coding.

## Architectural Philosophy: The Agnostic Daemon

In modern fleet architectures, the daemon itself is strictly decoupled from Endpoint Registries, API Routing, and Service Meshes. 

`delightd` compiles into a purely static, 15MB Go binary serving standard HTTP/REST. **It contains zero routing SDK bloat.** Because it is perfectly agnostic, it natively integrates with any API Gateway or Service Mesh your infrastructure prefers. 

We provide out-of-the-box Infrastructure-as-Code profiles for the two industry standards: **Traefik** and **Envoy**.

See [INSTALL.md](INSTALL.md) for detailed deployment profiles, and [DEVELOPERS.md](DEVELOPERS.md) for instructions on how to integrate your projects with the daemon.

## Binary & Docker Exports Engine

In addition to backups, `delightd` autonomously exposes executables and Docker shims from `~/work` into your `$PATH` via `~/var/bin`.

### Sensible Defaults (Zero-Touch)
`delightd` natively scans every managed project directory. If it finds a `bin/` subdirectory containing executable files, it automatically creates a direct symlink in `~/var/bin` pointing to them.

### Docker Wrappers (Shims)
For commands isolated inside Docker, `delightd` avoids polluting your `$PATH` with opaque binaries. Instead, it generates readable Bash scripts in its state directory (`~/var/runtime/delightd/exports/`) and symlinks those into `~/var/bin`. 

This provides a "procfs" like experience—you can simply `cat $(which my-docker-cli)` to see the exact syntax `delightd` is using to invoke the tool.

There are two supported Docker modes (configured in `~/etc/delight-registry.yaml`):

1. **`docker-run`**: Ephemeral execution (e.g. compilers/linters). The generated shim takes the form:
   ```bash
   #!/usr/bin/env bash
   exec docker run --rm -i -v "$(pwd):/workspace" -w /workspace <image> "$@"
   ```

2. **`docker-exec`**: Connecting to a running service (e.g. database cli). The generated shim takes the form:
   ```bash
   #!/usr/bin/env bash
   exec docker exec -i <container> <command> "$@"
   ```

### Idempotency & Archival
`delightd` never recklessly `rm`s footprints. If an export is removed, its symlink is archived to `~/var/archive/delightd/exports/<timestamp>/`, ensuring zero unintentional data loss while keeping `~/var/bin` clean.
