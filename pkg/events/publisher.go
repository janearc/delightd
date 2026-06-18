// Package events publishes delight.v1 events to Kafka, encoded in the Confluent
// Schema Registry protobuf wire format so standard Confluent consumers (and
// obs-svc) can deserialize them.
//
// TRADEOFFS vs confluent-kafka-go (librdkafka) -- read before trusting this:
//
// We use franz-go (twmb/franz-go), pure Go with no cgo, so delightd stays a
// static binary and the whole fleet stays one toolchain. The cost is that we
// hand-roll two things Confluent's official serializer would do for us, and we
// do NOT silently inherit its guarantees:
//
//  1. WIRE FRAMING. The Confluent protobuf format is
//     [magic 0x00][schema id, 4B big-endian][message-index varints][payload].
//     We build this ourselves in encode(). A bug here is SILENT: produce
//     succeeds, and only a consumer fails to deserialize. The official serde is
//     what normally protects you from this; here it's on us, hence the explicit
//     format notes on encode().
//
//  2. SCHEMA REGISTRATION + COMPATIBILITY. We register the schema over the SR
//     REST API under the RecordNameStrategy subject and cache the returned id.
//     We do NOT get the serialize-time compatibility enforcement the Confluent
//     serde performs; FULL_TRANSITIVE compatibility is enforced server-side by
//     the registry's configured level, not by this client.
//
// What we DO control and set explicitly (not librdkafka defaults): idempotent
// production with acks=all (franz-go's default when idempotency is on; stated
// here so it isn't assumed). And: a Kafka/SR outage must never block or fail a
// backup -- publishing errors are returned for the caller to log and move on.
package events

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	"google.golang.org/protobuf/proto"

	delightv1 "delightd/gen/go/delight/v1"
)

const (
	// backupEventSubject is the SR subject under RecordNameStrategy: the
	// fully-qualified protobuf message name.
	backupEventSubject = "delight.v1.BackupEvent"
	wireMagicByte      = 0x00
)

// Publisher produces delight.v1 events. A nil *Publisher is a valid no-op, so
// callers can hold one unconditionally and let a disabled/down Kafka be silent.
type Publisher struct {
	cfg        config
	client     *kgo.Client
	http       *http.Client
	schemaText string

	schemaID   int32
	registered bool
}

type config struct {
	brokers           []string
	schemaRegistryURL string
	topic             string
}

// New constructs a Publisher and connects the producer. schemaText is the
// PROTOBUF schema registered under the BackupEvent subject. A returned error
// means publishing is unavailable; the caller should log it and proceed with a
// nil Publisher rather than failing.
func New(ctx context.Context, brokers []string, schemaRegistryURL, topic, schemaText string) (*Publisher, error) {
	if len(brokers) == 0 {
		return nil, fmt.Errorf("no kafka brokers configured")
	}
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(brokers...),
		kgo.DefaultProduceTopic(topic),
		// explicit, not inherited: idempotent producer (franz-go default) with
		// all-ISR acks. stated so we don't assume librdkafka semantics.
		kgo.RequiredAcks(kgo.AllISRAcks()),
		kgo.ProducerLinger(5*time.Millisecond),
	)
	if err != nil {
		return nil, err
	}
	if err := cl.Ping(ctx); err != nil {
		cl.Close()
		return nil, fmt.Errorf("kafka unreachable: %w", err)
	}
	return &Publisher{
		cfg:        config{brokers: brokers, schemaRegistryURL: schemaRegistryURL, topic: topic},
		client:     cl,
		http:       &http.Client{Timeout: 5 * time.Second},
		schemaText: schemaText,
	}, nil
}

// Close releases the producer.
func (p *Publisher) Close() {
	if p != nil && p.client != nil {
		p.client.Close()
	}
}

// PublishBackup encodes and produces a BackupEvent. The schema is registered
// lazily on first use so a registry that is briefly down at startup self-heals.
// A nil Publisher is a no-op. Errors are returned for the caller to log;
// publishing must never block or fail the backup it describes.
func (p *Publisher) PublishBackup(ctx context.Context, ev *delightv1.BackupEvent) error {
	if p == nil {
		return nil
	}
	if !p.registered {
		id, err := p.registerSchema(ctx)
		if err != nil {
			return fmt.Errorf("schema registration: %w", err)
		}
		p.schemaID = id
		p.registered = true
	}

	frame, err := p.encode(ev)
	if err != nil {
		return err
	}
	rec := &kgo.Record{Topic: p.cfg.topic, Key: []byte(ev.GetProjectName()), Value: frame}
	return p.client.ProduceSync(ctx, rec).FirstErr()
}

// encode builds the Confluent protobuf wire format:
//
//	byte 0     : magic 0x00
//	bytes 1-4  : schema id, big-endian
//	byte 5     : message-index. BackupEvent is the FIRST message in
//	             delight.proto, so per the Confluent optimization the index
//	             array is the single byte 0x00 (rather than a [count][idx...]
//	             zig-zag varint list). If another message ever moves ahead of
//	             BackupEvent in the file, this must change or consumers break.
//	bytes 6+   : serialized protobuf payload
func (p *Publisher) encode(ev *delightv1.BackupEvent) ([]byte, error) {
	payload, err := proto.Marshal(ev)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 6+len(payload))
	out = append(out, wireMagicByte)
	var id [4]byte
	binary.BigEndian.PutUint32(id[:], uint32(p.schemaID))
	out = append(out, id[:]...)
	out = append(out, 0x00) // message-index: first message in the file
	out = append(out, payload...)
	return out, nil
}

// registerSchema POSTs the PROTOBUF schema under the RecordNameStrategy subject
// and returns the registry-assigned id. Idempotent server-side: re-registering
// an identical schema returns the existing id.
func (p *Publisher) registerSchema(ctx context.Context) (int32, error) {
	body, err := json.Marshal(map[string]string{"schemaType": "PROTOBUF", "schema": p.schemaText})
	if err != nil {
		return 0, err
	}
	url := fmt.Sprintf("%s/subjects/%s/versions", strings.TrimRight(p.cfg.schemaRegistryURL, "/"), backupEventSubject)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/vnd.schemaregistry.v1+json")

	resp, err := p.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("registry returned %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var out struct {
		ID int32 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.ID, nil
}
