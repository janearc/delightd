# Contract ownership

Every protobuf package on the mesh has exactly one owning repository. The owner's
`.proto` files are the source of truth; every other repository vendors from the owner
and regenerates. The owner's CI runs the authoritative `buf breaking` check for its
packages — a breaking change is caught where the contract lives, not where it lands.

## Owned by delightd

- `registry.v1` — project registration: how a service announces itself and how the
  roster is queried. Consumers (big-little-mesh's register-client, and magpie through
  it) vendor from here and regenerate. A change to `registry.v1` starts as a delightd
  PR.
- `resolve.v1` — the resolve surface (generated into the committed
  `delightd-contracts` Rust crate).

## Vendored here (owned elsewhere)

- `frood.v1` — owned by **big-little-mesh** (see its `docs/contract-ownership.md`).
- `delight.v1` — owned by **kafka-svc**, the canonical home of the bus contracts (see
  `docs/events.md`); vendored here for the `delight.events` producer.

## Why this document exists

Recorded 2026-07-02 as the pilot prerequisite of ADR-0001 (the coding-process gates):
a schema-breaking gate needs one source of truth to diff against, so package ownership
is pinned explicitly instead of living in commit-message archaeology.
