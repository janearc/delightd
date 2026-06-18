// Package delightproto embeds the vendored contract source so delightd can
// register it with the Schema Registry at runtime without a build-time copy or a
// filesystem dependency in the container. The .proto remains the vendored copy
// of kafka-svc's source of truth (see proto/README.md).
package delightproto

import _ "embed"

// BackupEventSchema is the PROTOBUF schema text registered under the
// delight.v1.BackupEvent subject (RecordNameStrategy).
//
//go:embed delight/v1/delight.proto
var BackupEventSchema string
