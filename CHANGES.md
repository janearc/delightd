# Changelog

All notable changes to delightd are recorded here. delightd follows semantic
versioning; its releases are coordinated with the blm release line.

## [Unreleased] — toward v0.5

- Adopt the shared `good-citizen` library for the git oracle / watcher, secrets,
  and Kafka emission (flag-day cutover; tracked in the **blm v0.5** milestone).
  delightd remains the source of truth and roster brain — it consumes the
  library, it is not a pipeline orchestrator.

## [0.4] — 2026-06-22

First tagged baseline. delightd is stable: the fleet's source of truth and the
brain that knows what is deployed, what is running, and what is safe to touch.

### Capabilities

- **Project registry** via `projects.d/` drop-in fragments, with fail-open
  config: a malformed fragment degrades the daemon (surfaced on `/health`)
  rather than aborting it.
- **Authoritative roster + git state** over HTTP (`GET /projects`, git status) —
  the roster the rest of the fleet reads, rather than each tool globbing the
  workspace.
- **Backup engine** with rotation, keyed by project, plus an introspection API.
- **git oracle / watcher**: a per-project poll loop that detects working-tree
  churn and drives a backup state machine.
- **cobra CLI**: the `delightd` daemon and `delightd lint`.
- **Skill + Traefik integration**: skill aggregation from project `mcp.json`,
  and Traefik dynamic-config writing.
- `/health`, `/metrics`, and green CI.
