package events

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/twmb/franz-go/pkg/kgo"
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

// recorder is the produce seam's test double: it captures every record produced.
type recorder struct {
	mu   sync.Mutex
	recs []*kgo.Record
}

func (r *recorder) produce(_ context.Context, rec *kgo.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, rec)
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.recs)
}

// srCapture records what the fake registry actually RECEIVED, so a test can
// assert a real registration happened — right method, right subject endpoint,
// right schema payload — not merely that the server was contacted (the
// "we heard of this guy" vs "we registered" distinction, PR 74 review).
type srCapture struct {
	mu     sync.Mutex
	method string
	path   string
	body   string
}

// srServer returns an httptest schema registry that counts every request,
// captures the last one's shape into seen, and answers with a fixed schema id.
func srServer(t *testing.T, id int32, registrations *int32, seen *srCapture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(registrations, 1)
		b, _ := io.ReadAll(r.Body)
		seen.mu.Lock()
		seen.method, seen.path, seen.body = r.Method, r.URL.Path, string(b)
		seen.mu.Unlock()
		w.Header().Set("Content-Type", "application/vnd.schemaregistry.v1+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":` + strconv.Itoa(int(id)) + `}`))
	}))
}

// TestPublishBackup_RegistersExactlyOnceUnderRace races several first publishes
// against the lazy registration path and asserts the registry is hit exactly once.
// Run under -race, it is the regression test for the unguarded registered/schemaID
// writes that PublishBackup used to make from detached goroutines.
func TestPublishBackup_RegistersExactlyOnceUnderRace(t *testing.T) {
	var registrations int32
	var seen srCapture
	sr := srServer(t, 9, &registrations, &seen)
	defer sr.Close()

	rec := &recorder{}
	p := &Publisher{
		cfg:        config{schemaRegistryURL: sr.URL, topic: "delight.events"},
		http:       sr.Client(),
		schemaText: "syntax = \"proto3\";",
		produce:    rec.produce,
	}

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			ev := &delightv1.BackupEvent{ProjectName: "paling", Success: true}
			if err := p.PublishBackup(context.Background(), ev); err != nil {
				t.Errorf("PublishBackup: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt32(&registrations); got != 1 {
		t.Errorf("schema registered %d times, want exactly 1", got)
	}
	if got := rec.count(); got != goroutines {
		t.Errorf("produced %d records, want %d", got, goroutines)
	}

	// registration took EFFECT, not just occurred: every produced frame must
	// carry the id the registry assigned — the "we registered" half (PR 74
	// review), proving all racing publishers consumed the one registration.
	rec.mu.Lock()
	for i, r := range rec.recs {
		if id := binary.BigEndian.Uint32(r.Value[1:5]); id != 9 {
			t.Errorf("record %d frame carries schema id %d, want 9 (the registered id)", i, id)
		}
	}
	rec.mu.Unlock()

	// and the registration itself was real: the right method, the
	// RecordNameStrategy subject endpoint, the schema payload we hold.
	seen.mu.Lock()
	if seen.method != http.MethodPost {
		t.Errorf("registry saw method %s, want POST", seen.method)
	}
	if want := "/subjects/delight.v1.BackupEvent/versions"; seen.path != want {
		t.Errorf("registry saw path %q, want %q", seen.path, want)
	}
	if !strings.Contains(seen.body, `"schemaType":"PROTOBUF"`) || !strings.Contains(seen.body, "proto3") {
		t.Errorf("registration body missing schema payload: %q", seen.body)
	}
	seen.mu.Unlock()
}

// TestPublishBackup_ProducesFramedRecord asserts the record reaching the produce
// seam carries the right topic, the project name as the key, and the Confluent
// frame shape (magic byte, schema id, message index) around the payload.
func TestPublishBackup_ProducesFramedRecord(t *testing.T) {
	var registrations int32
	var seen srCapture
	sr := srServer(t, 42, &registrations, &seen)
	defer sr.Close()

	rec := &recorder{}
	p := &Publisher{
		cfg:        config{schemaRegistryURL: sr.URL, topic: "delight.events"},
		http:       sr.Client(),
		schemaText: "syntax = \"proto3\";",
		produce:    rec.produce,
	}

	ev := &delightv1.BackupEvent{ProjectName: "paling", Success: true, BytesBefore: 1000, BytesAfter: 400}
	if err := p.PublishBackup(context.Background(), ev); err != nil {
		t.Fatalf("PublishBackup: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("produced %d records, want 1", rec.count())
	}

	got := rec.recs[0]
	if got.Topic != "delight.events" {
		t.Errorf("topic = %q, want %q", got.Topic, "delight.events")
	}
	if string(got.Key) != "paling" {
		t.Errorf("key = %q, want %q (project name)", got.Key, "paling")
	}
	if len(got.Value) < 6 {
		t.Fatalf("frame too short: %d bytes", len(got.Value))
	}
	if got.Value[0] != wireMagicByte {
		t.Errorf("magic byte = 0x%02x, want 0x00", got.Value[0])
	}
	if id := binary.BigEndian.Uint32(got.Value[1:5]); id != 42 {
		t.Errorf("schema id = %d, want 42 (the registered id)", id)
	}
	if got.Value[5] != 0x00 {
		t.Errorf("message-index = 0x%02x, want 0x00 (first message)", got.Value[5])
	}
}

// TestPublishBackup_NilIsNoOp: a nil Publisher is a valid no-op, so callers can
// hold one unconditionally when Kafka is disabled or down.
func TestPublishBackup_NilIsNoOp(t *testing.T) {
	var p *Publisher
	if err := p.PublishBackup(context.Background(), &delightv1.BackupEvent{ProjectName: "paling"}); err != nil {
		t.Errorf("nil PublishBackup returned %v, want nil", err)
	}
}
