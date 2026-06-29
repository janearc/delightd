# proto/

This directory holds two kinds of contracts, and the distinction matters because one is
re-synced destructively and the other is not.

## `delight/` — vendored from kafka-svc

`delight/` is a **vendored copy** of contracts owned by `kafka-svc`
(`~/work/kafka-logging/proto`), which is their single source of truth. delightd pins a copy
here and generates Go bindings from it at build time.

```sh
task sync-proto   # rm -rf proto/delight && copy kafka-svc's proto/delight over it
task generate     # regenerates gen/go from the proto
```

`task sync-proto` **wipes and replaces `proto/delight/`**, so keep those files
byte-identical to kafka-svc's originals — do not edit them here; drift is detectable and is
a bug. A contract change to `delight.v1` is made in kafka-svc and re-vendored.

## `registry/` — delightd-owned

`registry/` is **delightd's own** contract (the project taxonomy delightd manages). It is
not vendored, has no upstream, and `sync-proto` never touches it — `sync-proto` only
operates on `proto/delight`. Edit it here; this is its source of truth.

## Both

Generated bindings are never committed (`gen/` is gitignored; run `task generate`), the
same "no checked-in gencode" rule kafka-svc enforces. Managed mode retargets every
package's `go_package` to `delightd/gen/go/...`.
