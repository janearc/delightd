// Package enablement is delightd's per-project enable/disable state home.
//
// Doctrine, stated once: reads FAIL CLOSED. An absent record reads disabled.
// A store that could not open never pretends -- the HTTP surface serves 503
// degraded instead of an invented answer. This is deliberately the opposite
// of pkg/registry's degradation (empty set, carry on): the registry is
// additive, enablement gates actions. That difference is also why this is
// its own bbolt file rather than a bucket in the registry store -- one file
// must not carry two opposite failure doctrines.
package enablement

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"go.etcd.io/bbolt"
)

// bucket is the single bbolt bucket holding records, keyed by project name.
var bucket = []byte("enablement")

// The two states. Nothing more until it earns its place.
const (
	StateEnabled  = "enabled"
	StateDisabled = "disabled"
)

// Record is one project's enablement state plus the audit fields that answer
// the 03:20 question: who turned this off, and why.
type Record struct {
	Project   string    `json:"project"`
	State     string    `json:"state"`
	Reason    string    `json:"reason,omitempty"`
	Actor     string    `json:"actor"`
	ChangedAt time.Time `json:"changed_at"`
}

// Validate refuses anything but the two states, and requires a reason on
// disable -- a disabled project with no recorded why is an operational lie.
func (r Record) Validate() error {
	switch r.State {
	case StateEnabled:
	case StateDisabled:
		if r.Reason == "" {
			return fmt.Errorf("disable requires a reason")
		}
	default:
		return fmt.Errorf("unknown state %q (want %q or %q)", r.State, StateEnabled, StateDisabled)
	}
	if r.Project == "" {
		return fmt.Errorf("record has no project")
	}
	if r.Actor == "" {
		return fmt.Errorf("record has no actor")
	}
	return nil
}

// Store is the bbolt-backed state home. bbolt handles its own locking, so the
// Store needs no mutex (the registry pattern).
type Store struct {
	db *bbolt.DB
}

// Open opens (or creates) the store at path, creating parent directories.
// A failed Open is the fail-closed case: the caller leaves the HTTP surface
// unwired and /state serves 503 degraded -- the daemon still comes up.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("enablement: create store dir: %w", err)
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("enablement: open %s: %w", path, err)
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(bucket)
		return err
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("enablement: ensure bucket: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying bbolt file.
func (s *Store) Close() error { return s.db.Close() }

// Get returns the stored record for project. found=false means no record
// exists -- which the read surface renders as disabled (the store reports
// facts; the surface applies doctrine).
func (s *Store) Get(project string) (Record, bool, error) {
	var rec Record
	var found bool
	err := s.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucket).Get([]byte(project))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &rec)
	})
	return rec, found, err
}

// Put validates and upserts the record -- idempotent, last write wins, no
// compare-and-swap until a real race exists. ChangedAt is stamped here when
// the caller leaves it zero, so a bare PUT still carries its when.
func (s *Store) Put(rec Record) error {
	if err := rec.Validate(); err != nil {
		return err
	}
	if rec.ChangedAt.IsZero() {
		rec.ChangedAt = time.Now().UTC()
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucket).Put([]byte(rec.Project), b)
	})
}
