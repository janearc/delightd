# delightd — sentinels

Status: T0 design, awaiting operator ratification. This document proposes rulings;
it does not enact them. Rulings marked PROPOSED are not binding until ratified.

A sentinel is a change-admission gate that stands in front of an enclave of the mesh
and refuses a proposed change until the change is shown not to harm the enclave. It is
the coding-process judge (writer never judges its own diff; see
[contract-ownership.md](contract-ownership.md) and ADR-0001) promoted from process to
product: the same judgment, applied to the running mesh instead of a pull request, and
default-hostile. Where the judge asks "does this diff match its spec," the sentinel
asks "does the mesh still hold if this change is admitted." The sentinel is satisfied,
never bypassed, and satisfying it is expensive on purpose.

## 1. What a sentinel is, and the three gates

Three gate-shaped things exist in this system and they are easy to conflate. They gate
different things, at different times, in different places. The sentinel is only the
middle row.

| Gate | Gates | Fires when | Runs where |
|------|-------|-----------|-----------|
| Judge (ADR-0001) | what LANDS: a diff against its spec | a pull request is opened | process-side (sprints repo, CI) |
| Sentinel | what the MESH ACCEPTS as a change to its contracts and behavior | a change to an enclave is proposed | mesh-side (product; a delightd surface) |
| Enablement | what RUNS: runtime state of a project | a consumer reads project state | mesh-side (delightd `/state`) |

The boundary rule: the judge gates the diff, the sentinel gates the contract-and-behavior
change, enablement gates the running instance. A change can pass the judge (the diff is
correct against its spec) and still be refused by the sentinel (the mesh does not hold
under it). These are independent verdicts and MUST NOT be collapsed into one.

## 2. Doctrine (normative)

These three rules are the sentinel's whole posture. They are fail-closed applied to
change admission — the same doctrine delightd already applies to consumer reads (see
[availability.md](availability.md)): a wrong "yes" is worse than a slow "no."

- **Refusal is the default.** A sentinel MUST refuse a proposal it has not evaluated to a
  pass. Absence of a verdict is a refusal, not a pass. An evaluation that cannot
  complete (timeout, missing dependency, unreachable service) MUST refuse, and MUST cite
  what it could not reach.
- **Satisfied, never bypassed.** A proposal is admitted only by satisfying the sentinel.
  There MUST be no override flag, no "force" path, and no skip in the admission path. A
  human disagreeing with a refusal changes the sentinel's rules through review; they do
  not route around it for one change.
- **Expense is a ladder, not a flat rate.** The sentinel MUST run the cheapest screening
  that can rule on every proposal, and MUST escalate to more expensive tiers only when a
  cheaper tier cannot rule. The extraordinarily-expensive full-mesh evaluation SHOULD
  fire only when cheaper tiers return "cannot rule," never as the routine path. See
  section 4.

## 3. The first enclave: the contract surface

An enclave is a bounded slice of the mesh that one sentinel guards. The design starts
with exactly one, because the wire is already the mesh's boundary-enforcer and contract
changes are where damage concentrates: an off-contract message never arrives, so the
contract is the highest-leverage place to refuse a bad change before it exists.

The contract-surface enclave is:

- every `.proto` file in a repository's owned packages (see
  [contract-ownership.md](contract-ownership.md): `registry.v1` and `resolve.v1` are
  delightd's),
- the `buf` configuration that governs them (`buf.yaml`, `buf.gen.yaml`),
- every `gen/` tree generated from them (the committed bindings, in every language they
  are generated into).

**The mechanical floor.** Beneath the sentinel sit two required, non-judgmental checks
that either pass or fail deterministically:

| Check | What it proves |
|-------|----------------|
| `buf breaking` | the proposed `.proto` does not break the wire contract against the owned baseline |
| gen-drift | regenerate equals committed; no hand-edited generated code |

The floor is tier 0 (section 4). It is not the sentinel; it is the ground the sentinel
stands on. A proposal that fails the floor is refused before any judgment runs, because
a broken wire or drifted bindings make every higher judgment moot.

**What the sentinel judges above the floor.** The floor proves the change is mechanically
admissible. The sentinel judges whether it is *safe*: whether the dependency closure of
the changed contract still holds, whether the change is coherent with how the mesh uses
that contract, and whether the originator is in a state that can be trusted to ship it
(section 5).

**Admission of a new enclave.** A new enclave MUST be admitted by a demonstrated
operational need — a class of change that has caused or can cause concentrated damage and
has no gate — not by speculation. Each new enclave requires its own mechanical floor
(the deterministic checks that must hold) named before the sentinel judgment above it is
designed. Enforcement precedes construction: the enclave's floor MUST exist before the
jurisdiction it covers is built on.

## 4. The tier ladder

Every proposal enters at tier 0 and climbs only as far as it must. A tier that can rule
returns a verdict and stops the climb; a tier that cannot rule escalates.

| Tier | Runs on | Inputs | Pass means | Refusal means | Cost profile |
|------|---------|--------|-----------|---------------|--------------|
| 0 — mechanical floor | every proposal | proposed `.proto` + `buf` config + `gen/` trees | wire not broken, no gen drift | change is mechanically inadmissible | seconds; existing CI/buf tooling |
| 1 — cheap screening | every proposal that passed tier 0 | the diff, the contract's declared consumers, the change's stated intent | change is judged safe on cheap evidence | change is judged unsafe on cheap evidence | fast; SHOULD be free or near-free (candidate: local-model judge, sprints#38) |
| 2 — full-mesh evaluation | only proposals tier 1 cannot rule | the dependency closure, exercised services, originator state | mesh holds under the change | mesh does not hold, or cannot be shown to | extraordinarily expensive; on purpose (section 5) |

Tier 1 returns one of three answers, not two: **pass**, **refuse**, or **cannot rule**.
Only "cannot rule" escalates to tier 2. This three-valued result is what makes the ladder
cheap in the common case: a confident cheap verdict ends the evaluation, and the
expensive tier fires only where cheap evidence runs out. Tier 1 MUST be honest about its
own uncertainty — a tier 1 that returns "pass" when it should return "cannot rule"
defeats the ladder by admitting changes the expensive tier would have caught.

## 5. PROPOSED ruling (a): full-mesh evaluation, operationally

PROPOSED, awaiting operator ratification.

"Fully evaluating the mesh" (tier 2) is defined as three obligations, each of which the
verdict MUST cite evidence for:

- **Which services get exercised: the dependency closure of the changed contract.** The
  set is computed from the registry (`registry.v1`), not hand-listed: the direct
  consumers of the changed package, transitively, to the point where no further consumer
  depends on the changed surface. A service outside that closure is not exercised, because
  the contract change cannot reach it. The closure MUST be computed per-proposal against
  the live registry, not cached, for the same reason `GET /git` is computed live: a stale
  closure can miss a newly-added consumer.
- **What evaluating the originator covers.** The originator is the proposing service. Its
  evaluation covers: that it owns the package it proposes to change (contract ownership),
  that its own gen-freshness holds (it is not proposing from a drifted tree), and that its
  declared state is one the mesh admits changes from. An originator that cannot be
  established as the owner is refused; ownership is not assumed from the diff.
- **What evidence a verdict MUST cite.** A tier 2 verdict MUST cite, at minimum: the
  registry snapshot the closure was computed from, every service in the closure and the
  result of exercising it, the originator checks and their results, and, for a refusal,
  the specific service or check that could not be satisfied. A verdict that cannot name
  its evidence is a refusal, not a pass (section 2).

The tradeoff: computing the closure live and exercising it is the source of
the "extraordinarily expensive" cost the operator asked for. That cost is the point — it
is what makes a contract change to a live mesh a deliberate, evidenced act — but it means
tier 2 MUST NOT sit in any hot path, and the ladder (section 4) exists precisely so it
rarely runs.

## 6. PROPOSED ruling (b): verdict home and durability

PROPOSED, awaiting operator ratification.

The judge (ADR-0001) writes its verdicts to a ledger on the process side (the sprints
repo). The sentinel is mesh-side product, so its verdicts cannot live there without
coupling the running mesh to the process repository.

Proposed answer: **sentinel verdicts are a delightd-owned, durable record, emitted as a
mesh event and persisted by delightd, not written to the sprints ledger.** Concretely, a
verdict is a first-class record on a delightd surface (a `sentinel.v1` contract, owned by
delightd, is the natural home), durable across a delightd restart the way project state
is, and additionally emitted best-effort as an event on the bus the way `BackupEvent` is
(see [events.md](events.md)) so obs-svc and other consumers can observe admissions and
refusals without polling. The durability of record lives in delightd; the event is
telemetry and MUST NOT be the system of record.

The tradeoff: this creates a second verdict store distinct from the judge's
ledger, and the two must not be confused — the judge ledger records what landed, the
sentinel record records what the mesh admitted. The alternative, one shared ledger, was
rejected because it would couple the running mesh's admission record to the sprints
repository's availability and lifecycle, violating the same fail-closed independence that
keeps consumers from depending on anything but delightd itself (section 2,
[availability.md](availability.md)).

## 7. Whiteboard

The design half is above; this is the whiteboard half. Go-flavored pseudocode, not
compilable. The tier climb is a state machine, not if/else sprawl.

```go
// An Enclave is a bounded slice of the mesh one sentinel guards.
type Enclave struct {
    Name  string        // "contract-surface"
    Floor []MechCheck   // tier 0: deterministic, pass/fail (buf-breaking, gen-drift)
    // the judgment above the floor is the tier ladder, below.
}

// A Proposal is a proposed change to an enclave, from an originator.
type Proposal struct {
    Enclave    string
    Originator string   // the proposing service; established against registry.v1
    Diff       Diff     // proto + buf config + gen/ trees
    Intent     string   // the change's stated purpose (tier 1 input)
}

type Result int
const ( Pass Result = iota; Refuse; CannotRule ) // tier 1 is three-valued

// A Verdict is durable (delightd-owned) and cites its evidence (section 5).
type Verdict struct {
    Proposal  ProposalID
    Result    Result     // Pass or Refuse only; CannotRule is internal to the climb
    Tier      int        // the tier that ruled
    Evidence  []Evidence // registry snapshot, exercised services, originator checks
}

// The tier ladder as a state machine. Default is refusal (section 2):
// any state that cannot advance to a Pass resolves to Refuse.
type tierState int
const ( atFloor tierState = iota; atScreen; atFullMesh; decided )

func evaluate(p Proposal, e Enclave) Verdict {
    st := atFloor
    for st != decided {
        switch st {
        case atFloor: // tier 0, always
            // run every deterministic check; any failure -> refuse now.
            if !allPass(e.Floor, p) { return refuse(p, 0, floorEvidence) }
            st = atScreen
        case atScreen: // tier 1, cheap, every proposal
            // cheap judgment; candidate engine sprints#38 (must not depend on it).
            switch cheapScreen(p) {
            case Pass:       return pass(p, 1, screenEvidence)
            case Refuse:     return refuse(p, 1, screenEvidence)
            case CannotRule: st = atFullMesh // only "cannot rule" escalates
            }
        case atFullMesh: // tier 2, expensive, only when tier 1 cannot rule
            closure := registry.DependencyClosure(p.Enclave) // live, per-proposal
            ev := exercise(closure)                          // section 5
            ev = append(ev, checkOriginator(p.Originator)...)
            if !meshHolds(ev) { return refuse(p, 2, ev) }    // cannot show -> refuse
            return pass(p, 2, ev)
        }
    }
    return refuse(p, -1, nil) // unreachable; refusal is the floor of last resort
}
```

## 8. What this document does not decide

- **Additional enclaves.** Only the contract surface is admitted here. Any further enclave
  arrives by the admission rule in section 3 (demonstrated operational need, floor named
  first), through its own design, not this one.
- **Tier 2 implementation timeline.** The operational definition (section 5) is proposed;
  when and in what order tier 2 is built is a sprint-scoping question, not a T0 design
  question.
- **The tier 1 model choice.** Whether tier 1's cheap screening uses a local model, and
  which one, is the question of [sentinels-adr-0002-language.md](sentinels-adr-0002-language.md)
  and the sprints#38 experiment's data. This document names the local-model judge only as
  a candidate for the tier and MUST NOT be read as selecting it.
