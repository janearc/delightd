# Architectural Proposal: The `delight` System (`delightd`)

> [!IMPORTANT]
> `delightd` is a standalone daemon designed to manage and monitor Git-backed agent projects. It acts as a safety harness to minimize the blast radius of autonomous agents. For the rules governing the agents themselves, see `agent_behavior_policy.md`.

## 1. The Problem Space
Working with autonomous coding agents exposes the user to a massive blast radius. While traditional "tape backups" or nightly Time Machine snapshots work for human development speeds, agentic development is **extremely bursty**—weeks of zero activity followed by bursts of 2,500+ lines of code per day. If an agent hallucinates or makes a catastrophic destructive error, a nightly backup means losing an immense amount of high-density work. `delightd` solves this via Continuous Agentic Checkpointing (CAC).

## 2. Daemon Architecture
`delightd` is a persistent, lightweight daemon process.
- **Implementation**: Written in Python or TypeScript (Node.js) for rapid iteration and rich ecosystem support.
- **Telemetry and Control Port**: Exposes a clean REST/gRPC interface for dynamic configuration (adding projects, changing intervals) without restarting.
- **Alerting**: Integrates natively with macOS Notification Center to alert the user of high churn bursts, agent anomalies, or successful checkpoints.

## 3. Git-Driven Churn Detection
A core constraint of `delightd` is that **all managed directories must be Git projects**. 
Instead of heavily polling the filesystem or setting up complex file-watchers, `delightd` leverages Git as an optimized churn oracle.
- Polling runs aggressively (e.g., every 15 minutes or hourly).
- `delightd` executes `git status --porcelain` or analyzes uncommitted working tree diffs.
- **Fallow Periods**: If the working tree is clean or unchanged since the last poll, the daemon sleeps.
- **Bursty Periods**: If significant churn is detected, `delightd` triggers the backup pipeline.

## 4. Manifest-Driven Rotational Backups
When the event loop detects churn, `delightd` does not blindly execute `tar`. It uses a manifest-driven, logrotation-style approach.

### The Manifest Pipeline
1. **Discovery (`find`)**: The system traverses the directory structure using `find` (or native equivalents) to build a strict manifest of files to back up.
2. **Exclusions**: It actively excludes compiled binaries, object files, and known heavy directories via `-type` and `-name` flags (and by natively respecting `.gitignore`).
3. **Archiving**: The generated manifest is piped directly to `tar` (e.g., `tar cf - -T manifest.txt | bzip2 > archive.tbz`) or handled natively via Python's `tarfile` library using the pre-filtered list.

### Rotation and Deduplication
- **Logrotation Style**: Archives are stored in a structured logrotation format (e.g., `~/var/backups/paling/paling-20260613-1400.tbz`).
- **Deduplication**: `delightd` maintains state. It will not create a new `.tbz` archive if the manifest hashes and file diffs match the previous checkpoint. 
- **Retention**: Older checkpoints are rolled off based on a configurable max-archive limit, ensuring disk space remains svelte.

## 5. Configuration (`delight.yaml`)
The daemon is initialized via a declarative YAML file mapping out the Git projects it owns:

```yaml
# delight.yaml
system:
  root: "~/var"
  config_root: "~/etc"
  daemon:
    control_port: 8080
    pid_file: "~/var/run/delightd.pid"

projects:
  - name: "paling"
    path: "~/work/paling"
    backup:
      check_interval: "15m"   # High-frequency polling for bursty agent work
      rotation:
        max_archives: 48      # Keep 48 checkpoints (e.g., 12 hours of rapid 15m churn)
```
