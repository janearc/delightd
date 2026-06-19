# delightd — availability contract

delightd is the fleet's control plane. Its consumers do not work without it, and
that is by design. This document states the contract so no consumer is written
to hedge against delightd's absence — the resilience belongs in delightd, not in
its consumers.

## The rule

> Consumers fail closed. There is no local fallback. Resilience lives in delightd
> coming up in any condition, not in consumers hedging against it being absent.

## What "fail closed" means here

fleet-svc is the primary consumer. When an operator runs fleet's `git status`
over the fleet, fleet calls delightd's `GET /git`. If delightd does not answer:

- fleet returns an **error**. It does **not** fall back to shelling out to `git`
  itself, and it does **not** return a partial or guessed answer.
- The operator sees that the control plane is down. They do not see a stale or
  fabricated "everything is clean."

This matters because the answer gates destructive action. fleet gates
host-migration teardown on the dirty/unpushed reading. A wrong "clean" is worse
than no answer: it can greenlight a teardown over uncommitted work. A consumer
that fabricated an answer when delightd was down would defeat the entire point of
having a single authority for git-state.

This is also why `GET /git` is computed **live, per request** with go-git rather
than served from a cache — a cache could hand back a stale "clean" after the tree
went dirty. See [api.md](api.md#get-git) for the sweep behavior.

## Deploy-before-use

Because consumers have no fallback, the consumer-facing surface is a hard
dependency edge:

- A change to the surface consumers read (most importantly `GET /git`, its
  shape, or its semantics) requires a **new delightd binary to be deployed**
  before the dependent fleet commands work.
- There is no client-side reimplementation to limp along on, and no compatibility
  shim layer. The contract is the running daemon's contract.
- Order of operations for any such change: land and deploy the delightd binary,
  then the consumer change that depends on it. Not the reverse.

## Where the resilience actually lives

Since consumers cannot route around delightd, the engineering effort goes into
delightd starting and serving under adverse conditions, not into consumers
tolerating its absence:

- **No external hard dependency at startup.** Kafka and the Schema Registry are
  optional; with no brokers configured (or unreachable ones) the daemon runs
  exactly as before and event emission is a no-op. See [events.md](events.md).
- **Per-project isolation in the sweep.** One unreadable or slow project cannot
  take down `GET /git`: each project read is bounded by a 5 s deadline and its
  failure is reported in-band, never aborting the sweep.
- **The control loop degrades, it does not crash.** A failed export sync, a
  failed LLM probe, or a failed backup is logged and the loop continues; a
  backup failure moves only that project's state machine into error backoff.

The operational corollary: keep delightd up. A delightd that is reliably present
is the whole availability story, because nothing downstream is built to survive
without it.
