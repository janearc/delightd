# proto/

This directory holds two kinds of contracts, and the distinction matters because the
vendored ones are re-synced destructively and the owned ones are not.

## Vendored — do not edit here

These are **vendored copies** of contracts owned upstream; `task sync-proto` wipes and
replaces each from its source, so keep them byte-identical to their originals (drift is
detectable and is a bug). A change to a vendored contract is made upstream and re-vendored.

- `delight/` — owned by `kafka-svc` (`~/work/kafka-logging/proto`). The bus event contracts
  delightd emits; kafka-svc owns the bus schemas, so delightd vendors the copy it generates from.
- `frood/` — owned by `big-little-mesh` (`~/work/big-little-mesh/proto/frood`). The guaranteed frood interface
  (`frood.v1.Identity` / `ContractDescriptor`) that delightd's `registry.v1` register wire
  references.

```sh
task sync-proto   # rm -rf + recopy proto/delight (from kafka-svc) and proto/frood (from big-little-mesh)
task generate     # regenerates gen/go from the proto
```

### Why `frood.v1` is vendored here

This one is not obvious top-to-bottom: it is a contract delightd does not own, copied in and
then referenced by a contract delightd *does* own (`registry.v1`). The reason is ownership.
`frood.v1` is the universal frood interface — the set *every* frood on the mesh
implements, a `frood` concept that belongs to big-little-mesh, not to delightd. delightd's
`/register` broker has to speak it: `registry.v1.RegisterRequest` carries the registering
frood's `frood.v1.Identity` and `ContractDescriptor`. So delightd vendors a byte-identical
copy and imports it from `registry.v1`, the same generate-at-build way it vendors `delight.v1`.
Redefining the interface here would fork a contract big-little-mesh owns; a cross-repo buf-module
dependency is heavier than this repo needs. The split holds: big-little-mesh owns the frood interface,
delightd owns the register protocol that uses it, and the copy under `frood/` is where the
two meet without either side owning the other's contract.

## `registry/` — delightd-owned

`registry/` is **delightd's own** contract: the project taxonomy delightd manages
(`project.proto`) and the `/register` broker wire (`register.proto`, which references the
vendored `frood.v1`). It has no upstream, and `sync-proto` never touches it. Edit it here;
this is its source of truth.

## Both

Generated bindings are never committed (`gen/` is gitignored; run `task generate`), the
same "no checked-in gencode" rule kafka-svc enforces. Managed mode retargets every
package's `go_package` to `delightd/gen/go/...`.
