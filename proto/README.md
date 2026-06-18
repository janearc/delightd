# proto/ — vendored contracts

These `.proto` files are **vendored copies** of the contracts owned by `kafka-svc`
(`~/work/kafka-logging/proto`), which is the single source of truth. delightd
pins a copy here and generates Go bindings from it at build time — the bindings
themselves are never committed (`gen/` is gitignored; run `task generate`), the
same "no checked-in gencode" rule kafka-svc enforces.

To refresh after a contract change in kafka-svc:

```sh
task sync-proto   # copies kafka-svc's proto/ over this directory
task generate     # regenerates gen/go from the vendored proto
```

Keep these files byte-identical to kafka-svc's originals so drift is detectable.
