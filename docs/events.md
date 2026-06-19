# delightd — Kafka event contract

After each checkpoint attempt, delightd emits a `delight.v1.BackupEvent` to the
`delight.events` topic, encoded in the Confluent Schema Registry protobuf wire
format so standard Confluent consumers — and obs-svc — can deserialize it. It is
the fleet's first Kafka producer, so the conventions here are the ones later
producers follow. The implementation is `pkg/events`; this document is the
contract.

## Best-effort, always

Event emission never blocks or fails a backup:

- With no brokers configured, the publisher is `nil` and every publish is a
  silent no-op. The daemon runs exactly as it did before Kafka existed.
- If the producer fails to initialize (brokers unreachable at startup), the
  daemon logs it and proceeds with a `nil` publisher.
- A publish error at runtime is returned to the caller, logged, and dropped. The
  backup it describes has already happened; the event is telemetry, not a commit.
- The schema is registered **lazily on first publish**, so a Schema Registry that
  is briefly down at startup self-heals on the next event.

A `nil *Publisher` is a valid no-op receiver, so the daemon holds one
unconditionally and lets a disabled or down Kafka stay silent.

## Why franz-go (the tradeoff, stated plainly)

The publisher uses **franz-go** (`twmb/franz-go`), pure Go with no cgo, so
delightd stays a static binary and the whole fleet stays on one toolchain. The
alternative, confluent-kafka-go, wraps librdkafka (cgo) and ships its own
protobuf serializer. Choosing franz-go means we hand-roll two things that the
Confluent serializer would otherwise do, and we do **not** silently inherit its
guarantees:

1. **Wire framing.** We build the Confluent protobuf frame ourselves. A bug here
   is *silent*: the produce succeeds and only a consumer fails to deserialize.
   The framing is documented below and pinned by the `encode()` notes in code.
2. **Schema registration + compatibility.** We register the schema over the SR
   REST API and cache the returned id. We do **not** get the serialize-time
   compatibility check the Confluent serde performs; compatibility is enforced
   server-side by the registry's configured level, not by this client.

What the producer sets **explicitly** (not inherited librdkafka defaults):
idempotent production with `acks=all` (all-ISR), and a 5 ms producer linger.

## Wire format

The value of each record is the Confluent protobuf frame:

```text
byte 0     : magic 0x00
bytes 1-4  : schema id, big-endian
byte 5     : message-index = 0x00
bytes 6+   : serialized protobuf payload
```

The single `0x00` message-index byte is the Confluent optimization for "the
first message in the file." `BackupEvent` is the first message in
`delight.proto`. **If another message is ever moved ahead of `BackupEvent` in
that file, this byte must change or every consumer breaks.**

The record **key** is the project name (`BackupEvent.project_name`), so events
for one project land on a single partition in order.

## Schema and subject

| Property | Value |
|----------|-------|
| Topic | `delight.events` |
| Subject (RecordNameStrategy) | `delight.v1.BackupEvent` |
| Schema type | `PROTOBUF` |
| Registration | `POST {schema_registry_url}/subjects/delight.v1.BackupEvent/versions`, lazy, idempotent server-side |

RecordNameStrategy means the subject is the fully-qualified message name, not
`<topic>-value` — so multiple event types can share the `delight.events` topic,
each under its own subject, as the fleet adds producers.

## The message

`BackupEvent` (from `proto/delight/v1/delight.proto`, vendored from kafka-svc):

```protobuf
message BackupEvent {
  string project_name = 1;
  bool success = 2;
  uint64 bytes_before = 3;            // total size of included files, pre-compression
  uint64 bytes_after = 4;            // size of the written .tgz
  uint32 duration_milliseconds = 5;  // wall-clock checkpoint time
  google.protobuf.Timestamp timestamp = 6;
}
```

| Field | Source |
|-------|--------|
| `project_name` | the project being checkpointed |
| `success` | whether the checkpoint succeeded |
| `bytes_before` | `CheckpointResult.BytesBefore` (sum of included regular-file sizes) |
| `bytes_after` | `CheckpointResult.BytesAfter` (`.tgz` size; `0` on failure) |
| `duration_milliseconds` | `CheckpointResult.Duration` |
| `timestamp` | emit time (`timestamppb.Now()`) |

On a failed checkpoint the daemon emits the event with `success: false` and a
zero `CheckpointResult` (so `bytes_after` is `0`).

## Configuration

```yaml
system:
  kafka:
    brokers: ["kafka:9092"]
    schema_registry_url: "http://schema-registry:8081"
    topic: "delight.events"
```

Empty `brokers` disables the producer (see "best-effort, always"). The env
overrides are `DELIGHT_SYSTEM_KAFKA_BROKERS`,
`DELIGHT_SYSTEM_KAFKA_SCHEMA_REGISTRY_URL`, `DELIGHT_SYSTEM_KAFKA_TOPIC`
(see [operations.md](operations.md)).

## Proto ownership

The `.proto` is **vendored from kafka-svc** (`~/work/kafka-logging/proto`), the
single source of truth. delightd pins a copy under `proto/` and generates Go
bindings at build time; the bindings are never committed. Refresh with
`task sync-proto && task generate`. See [proto/README.md](../proto/README.md).

> Topic and subject **provisioning** (creating the topic, setting the registry's
> compatibility level) is a kafka-svc concern, not delightd's. delightd produces;
> it does not administer the cluster.
