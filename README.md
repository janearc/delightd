# Delightd: Autonomous Hyperscale Checkpoint Daemon

`delightd` is a lightweight, zero-dependency Go daemon designed to continuously poll massive Git repositories natively in-memory, evaluate churn, and compress local `.tgz` snapshot archives to prevent catastrophic data loss during autonomous burst coding.

## Architectural Philosophy: The Agnostic Daemon

In modern fleet architectures, the daemon itself is strictly decoupled from Endpoint Registries, API Routing, and Service Meshes. 

`delightd` compiles into a purely static, 15MB Go binary serving standard HTTP/REST. **It contains zero routing SDK bloat.** Because it is perfectly agnostic, it natively integrates with any API Gateway or Service Mesh your infrastructure prefers. 

We provide out-of-the-box Infrastructure-as-Code profiles for the two industry standards: **Traefik** and **Envoy**.

### Starting the Fleet Stack

You can inject the endpoint registry of your choice at runtime without altering the underlying Go binary:

#### 1. Traefik (The Dynamic Auto-Discovery Route)
Traefik binds to the Docker socket and automatically registers `delightd` dynamically via container labels. No bespoke configuration files required.
```bash
docker-compose --profile traefik up -d
```
*Agents can route traffic directly through `http://delightd.local`.*

#### 2. Envoy (The Explicit Hyperscaler Route)
For fleets standardizing on Envoy proxying and rigid configurations.
```bash
docker-compose --profile envoy up -d
```

### Mount Invariants & Root Protection
The container strictly bounds its execution user to your host `UID/GID`. This ensures all `.tgz` archives it creates in `/work/backups` are safely owned by your specific host user and never by `root`, preventing local workstation permissions corruption.
