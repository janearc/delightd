# delightd — ADR-0002: sentinel tier language

Status: DRAFT, awaiting operator ratification.

## 1. Context

The sentinels design ([sentinels.md](sentinels.md)) defines a change-admission gate over
mesh enclaves, structured as a three-tier ladder. The implementation language is an ADR
question, not an assumption: the assessment core is judge-shaped work, and the tiers do
different work at different cost, so a single blanket answer may be wrong per tier.

Go is the null hypothesis: it is the fleet's control-plane language of record, delightd is
one Go binary (see [architecture.md](architecture.md)), and a sentinel is a delightd
surface. The standing fleet doctrine that decides this is that the wire is the mesh's
boundary-enforcer, not in-language validation: any language can join the mesh behind a
proto contract, so a language choice for one component never dictates another's.

The tiers, and what each actually asks of a language:

| Tier | Work | Language pressure |
|------|------|-------------------|
| 0 — mechanical floor | run `buf breaking` and gen-drift checks | none new; existing CI/buf toolchain |
| 1 — cheap screening | fast, near-free judgment on cheap evidence | wants fast local inference; sprints#38 (Apple-silicon local-model judge) may pull Apple-frameworks work into scope |
| 2 — full-mesh evaluation | compute the dependency closure, exercise it, assemble evidence | orchestration + registry access + evidence assembly: control-plane work |

Tier 0 introduces no new language: it is the `task` plus `buf` toolchain delightd already
runs at build time. Tier 2 is squarely control-plane Go: it reads `registry.v1`, walks the
dependency closure, drives service evaluation, and assembles the cited evidence a verdict
requires. Tier 1 is the only open pressure: cheap-and-fast screening may want local model
inference, and the sprints#38 experiment may show that the best available local inference
lives behind Apple frameworks, which are not reachable from Go in-process.

## 2. Decision

- All sentinel **service code is Go** — the tier ladder, the state machine, registry
  access, closure computation, evidence assembly, verdict persistence and emission. This
  MUST be Go, consistent with delightd being one Go binary.
- Tier 0 MUST use the existing `buf` and `task` toolchain. It introduces no new language.
- Tier 1 **model inference, if it lands, arrives behind a proto contract as a sidecar
  frood.** The sentinel service calls it over the wire; the inference process MAY be in
  any language. Its language is decided by the sprints#38 outcome, not by this ADR. The
  sentinel MUST NOT depend on that sidecar existing: tier 1 without it degrades to a
  Go-native cheap screen or to "cannot rule," which escalates per the ladder.

This keeps the language decision where the evidence is. Go is chosen for everything the
mesh's control plane already dictates; the one genuinely open question — what runs cheap
local inference — is deferred to the experiment built to answer it, and isolated behind a
contract so answering it later changes nothing in the sentinel's own code.

## 3. Consequences

- The sentinel stays a Go component in the delightd binary; no new toolchain enters the
  service itself. Tier 0 reuses what is already built.
- Tier 1's inference engine is swappable without touching the sentinel: because it is a
  sidecar frood behind a proto contract, its language can change, or it can be absent, and
  the sentinel's admission logic is unaffected. The wire absorbs the language boundary,
  which is the doctrine that lets any language join the mesh.
- The sentinel MUST handle the sidecar being absent as a normal condition, not an error:
  tier 1 degrades rather than failing the whole ladder. This matches delightd's
  best-effort posture toward optional dependencies (see [events.md](events.md)).
- A `sentinel.v1` contract (verdicts, and the tier-1 inference call if it lands) becomes a
  delightd-owned package, subject to the same contract-ownership rules as
  `registry.v1` and `resolve.v1` (see [contract-ownership.md](contract-ownership.md)).

What would reopen this ADR:

- The sprints#38 experiment showing local inference is best run in-process in Go: the
  sidecar seam for tier 1 would be unnecessary and could be removed.
- A tier growing work that Go serves poorly and that cannot be isolated behind a contract:
  that would be a genuine reason to revisit the all-Go service decision, which no tier
  currently presents.
- A new enclave (per [sentinels.md](sentinels.md) section 3) whose floor or judgment needs
  tooling outside the Go and `buf` toolchain.
