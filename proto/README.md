# proto/

This directory holds two kinds of contracts, and the distinction matters because the
vendored ones are re-synced destructively and the owned ones are not.

## Vendored — do not edit here

These are **vendored copies** of contracts owned upstream; `task sync-proto` wipes and
replaces each from its source, so keep them byte-identical to their originals (drift is
detectable and is a bug). A change to a vendored contract is made upstream and re-vendored.

- `delight/` — owned by `kafka-svc` (`~/work/kafka-logging/proto`). The bus event contracts.
- `citizen/` — owned by `blm` (`~/work/blm/proto/citizen`). The guaranteed citizen interface
  (`citizen.v1.Identity` / `ContractDescriptor`) that delightd's `registry.v1` register wire
  references.

```sh
task sync-proto   # rm -rf + recopy proto/delight (from kafka-svc) and proto/citizen (from blm)
task generate     # regenerates gen/go from the proto
```

## `registry/` — delightd-owned

`registry/` is **delightd's own** contract: the project taxonomy delightd manages
(`project.proto`) and the `/register` broker wire (`register.proto`, which references the
vendored `citizen.v1`). It has no upstream, and `sync-proto` never touches it. Edit it here;
this is its source of truth.

## Both

Generated bindings are never committed (`gen/` is gitignored; run `task generate`), the
same "no checked-in gencode" rule kafka-svc enforces. Managed mode retargets every
package's `go_package` to `delightd/gen/go/...`.
