# Architectural Invariants: delightd

## 1. Overview
`delightd` is a continuous agentic checkpointing (CAC) state machine. It mitigates destructive AI failures by orchestrating manifest-driven `.tgz` archiving based on filesystem churn metrics. 

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
