# delightd and fleet: one control plane

A control plane is only as trustworthy as its source of truth, and a source of
truth is only true if there is one of it. delightd already answers what is true
about a project — its git state, its backup state, what it exposes — and the
rest of the fleet fails closed when it cannot reach delightd (see
[availability.md](availability.md)). But there is one fact delightd does not yet
own, and it is the most basic one: the list of projects itself.

Today that list lives in two places. delightd keeps its own `projects:` registry
in `delight.yaml`. fleet-svc keeps its own `repositories:` roster in
`WorkstationConfig.yaml`. They name the same projects, each carries fields the
other lacks, and nothing holds them in step. Two registries for one fleet is two
answers to "what are we running" — the one question a control plane exists to
answer exactly once.

This document records the decision to make delightd the single source of that
truth, and to stop treating delightd and fleet-svc as two programs that happen to
talk.

## The shape: one brain, two hands

delightd is the brain: it holds what is true and decides nothing about how the
fleet acts. fleet-svc is the hands: it converges the machine, rolls deployments,
and gates teardowns — always by asking the brain first. The fail-closed contract
already encodes this. fleet-svc's deploy refuses to run when delightd is
unreachable ("refusing to deploy without the source of truth"), and its
host-migration guard blocks on delightd's dirty/unpushed answer. The hands
already will not move without the brain.

What they are not, yet, is one body. delightd is Go; fleet-svc is Python; they
share no code and keep separate registries. The decision here closes that gap:
**fleet-svc becomes a wrapper around delightd — one Go codebase, delightd as the
source-of-truth core, fleet-svc as the actuation layer that drives it.**

## How we grew two registries

Neither registry was a mistake at the time. delightd needed a project list to
know what to checkpoint and whose git tree to read, so it grew one in
`delight.yaml`. fleet-svc needed a roster to know what to install, converge, and
tear down, so it grew one in `WorkstationConfig.yaml`. Each was the smallest
thing that worked for the job in front of it. They became a liability only once
both were load-bearing: a project added to one and not the other is a project the
fleet half-knows, and "delightd is reporting a stale roster" is a sentence the
code already has to say.

## The decision

- **One codebase.** fleet-svc's commands move into delightd's repository and wrap
  its core. This is a Python→Go rewrite of fleet-svc, chosen deliberately: the
  control plane belongs in Go (delightd already is; fleet-svc is the lone
  exception), and one language over one source of truth removes the seam where
  the two drift.
- **delightd owns the roster.** The authoritative list of projects — and the
  fields fleet needs to act on them — lives in delightd's registry. fleet reads
  it; it keeps none of its own.
- **One brain to bring back.** This is much of why the roster lives in delightd
  at all. From a cold machine, delightd is what prepares the host and orchestrates
  it forward — installing what's needed, standing up the cluster, deploying the
  workloads — until you have a working environment again. That path wants one
  place to ask "what was running here"; two registries are two things to restore
  and reconcile, and one is one.

This is a direction, not a flag day. The rewrite lands as a series of reviewable
changes, never a single cutover.

## The seam: the first step, and the only structural change today

The first cut is the roster itself.

- delightd's project registry absorbs the fields it does not yet carry — each
  project's `essential` tier and its `deploy` kind (compose / kube / launchd) —
  alongside the `name`, `path`, and live `remote_url` it already has.
- delightd exposes the roster as a listing: `GET /projects` returns every managed
  project with those fields. Today the daemon has `GET /projects/{name}/…` per
  project and lists them only implicitly under `GET /git`; this makes membership
  itself a first-class, queryable surface.
- fleet-svc reads `GET /projects` for its lifecycle, bootstrap, and tier-0
  classification, instead of parsing `WorkstationConfig.yaml`.
- `WorkstationConfig.yaml` keeps what is genuinely the *workstation's*: brew
  packages and casks, daemons, the container runtime, the model set. The
  `repositories:` block — which was never the workstation's to own — retires.

As of today, the registries are still two, and this document is the plan to make
them one, starting at the roster. When the seam lands, "what are we running" has
exactly one answer, and the Python→Go consolidation proceeds behind it.
