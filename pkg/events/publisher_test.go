package events

import (
	"encoding/binary"
	"testing"

	"google.golang.org/protobuf/proto"

	delightv1 "delightd/gen/go/delight/v1"
)

// TestEncode_ConfluentWireFormat pins the hand-rolled framing. Because we own
// this (rather than Confluent's serde), a regression here is otherwise silent
// until a consumer fails to deserialize.
func TestEncode_ConfluentWireFormat(t *testing.T) {
	p := &Publisher{schemaID: 7}
	ev := &delightv1.BackupEvent{
		ProjectName: "paling",
		Success:     true,
		BytesBefore: 1000,
		BytesAfter:  400,
	}

	frame, err := p.encode(ev)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(frame) < 6 {
		t.Fatalf("frame too short: %d bytes", len(frame))
	}

	if frame[0] != wireMagicByte {
		t.Errorf("magic byte = 0x%02x, want 0x00", frame[0])
	}
	if id := binary.BigEndian.Uint32(frame[1:5]); id != 7 {
		t.Errorf("schema id = %d, want 7", id)
	}
	if frame[5] != 0x00 {
		t.Errorf("message-index = 0x%02x, want 0x00 (first message)", frame[5])
	}

	var got delightv1.BackupEvent
	if err := proto.Unmarshal(frame[6:], &got); err != nil {
		t.Fatalf("payload did not round-trip: %v", err)
	}
	if got.GetProjectName() != "paling" || !got.GetSuccess() || got.GetBytesBefore() != 1000 || got.GetBytesAfter() != 400 {
		t.Errorf("round-tripped event mismatch: %+v", &got)
	}
}
