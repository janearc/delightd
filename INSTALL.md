# delightd: Installation & Deployment

`delightd` compiles into a microscopic scratch container. It requires strictly mapped volumes and host user alignment to prevent filesystem corruption.

## 1. Mount Invariants & Root Protection

The container strictly bounds its execution user to your host `UID/GID`. This ensures all `.tgz` archives it creates, and all shell scripts it writes to `~/var/`, are safely owned by your specific host user and never by `root`. 

**Required Volumes**:
- `~/work:/work:rw` (For monitoring and backing up repositories)
- `~/etc/delightd:/etc/delightd:ro` (For reading global configuration and registries)
- `~/var:/var:rw` (For writing CLI binary symlinks and Docker shims)

## 2. Starting the Fleet Stack

You can inject the endpoint registry of your choice at runtime without altering the underlying Go binary:

### Profile: Traefik (The Dynamic Auto-Discovery Route)
Traefik binds to the Docker socket and automatically registers `delightd` dynamically via container labels. No bespoke configuration files required.
```bash
docker-compose --profile traefik up -d --build
```
*Agents can route traffic directly through `http://delightd.local`.*

### Profile: Envoy (The Explicit Hyperscaler Route)
For fleets standardizing on Envoy proxying and rigid configurations.
```bash
docker-compose --profile envoy up -d --build
```

### Profile: Standalone (Dry-Run Testing)
To verify `delightd` behavior safely before allowing it to write archives or symlinks:
```bash
docker-compose up -d --build delightd
# Ensure command array in docker-compose.yml includes "--dry-run"
```
