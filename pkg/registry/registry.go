// Package registry holds delightd's set of live frood registrations -- the state the
// /register broker maintains. It sits ALONGSIDE the static yaml/poll roster and does not
// replace it; nothing in the broker is required yet.
//
// Persistence is bbolt: registrations live in a single bucket keyed by project name, and
// Put/Upsert/Delete are transactions. bbolt gives us atomicity (the collision-checked Upsert
// is one transaction, so the check and the write cannot interleave) and durability
// (warm-start is just reopening the file -- no hand-rolled snapshot) natively. The warm
// cache is provisional, not the source of truth: a frood's lease (a later step) expires it
// if it stops renewing. Each value is the protojson form of registry.v1.Registration, so the
// stored form matches the wire shape.
package registry

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.etcd.io/bbolt"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	registryv1 "delightd/gen/go/registry/v1"
)

// bucket is the single bbolt bucket holding registrations, keyed by project name.
var bucket = []byte("registrations")

// marshal encodes a Registration as protojson with proto field names, so the stored value
// matches the GET /registrations wire shape.
var marshal = protojson.MarshalOptions{UseProtoNames: true}

// ErrEndpointHeld means a DIFFERENT project's live registration already holds the endpoint
// address an Upsert tried to claim. The caller gets the holding project's name back.
var ErrEndpointHeld = errors.New("endpoint already held by another project")

// Lease policy. A registration is held by a lease that the frood's heartbeat refreshes; it
// expires unless refreshed. The policy lives here, with the leases.
const (
	// DefaultLeaseTTL is how long a registration holds after a heartbeat (or a fresh
	// /register) before it must be refreshed again.
	DefaultLeaseTTL = 90 * time.Second
	// WarmStartGrace is the short lease re-stamped onto registrations loaded from the store
	// on boot: a frood that died while delightd was down is expired shortly after boot
	// unless its heartbeat refreshes the lease first.
	WarmStartGrace = 30 * time.Second
	// DefaultSweepInterval is how often the expiry sweep runs.
	DefaultSweepInterval = 15 * time.Second
)

// Registry is the bbolt-backed set of frood registrations. bbolt handles its own locking,
// so the Registry needs no mutex.
type Registry struct {
	db  *bbolt.DB
	log *slog.Logger
}

// Open opens (creating if needed) the bbolt-backed registry at path and ensures the bucket
// exists. Warm-start is implicit: registrations a prior process wrote are already present.
// The caller MUST Close it. bbolt takes a file lock, so a second opener fails after Timeout
// rather than corrupting the store.
func Open(path string, log *slog.Logger) (*Registry, error) {
	if log == nil {
		log = slog.Default()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		_, e := tx.CreateBucketIfNotExists(bucket)
		return e
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	log.Info("registry: opened bbolt store", "path", path)
	return &Registry{db: db, log: log}, nil
}

// Close releases the bbolt file lock.
func (r *Registry) Close() error { return r.db.Close() }

// Put records or replaces a project's registration (one per project) in a transaction.
func (r *Registry) Put(reg *registryv1.Registration) error {
	b, err := marshal.Marshal(reg)
	if err != nil {
		return err
	}
	return r.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucket).Put([]byte(reg.GetProject()), b)
	})
}

// Upsert records reg unless a DIFFERENT project already holds reg's endpoint address, in
// which case it makes no change and returns the holding project's name with ErrEndpointHeld.
// The same project re-claiming its own endpoint is idempotent. The collision check and the
// write are one bbolt transaction, so two froods racing for one address cannot both win.
func (r *Registry) Upsert(reg *registryv1.Registration) (string, error) {
	b, err := marshal.Marshal(reg)
	if err != nil {
		return "", err
	}
	addr := reg.GetEndpoint().GetAddress()
	var holder string
	err = r.db.Update(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(bucket)
		if addr != "" {
			c := bkt.Cursor()
			for k, v := c.First(); k != nil; k, v = c.Next() {
				if string(k) == reg.GetProject() {
					continue
				}
				var existing registryv1.Registration
				if err := protojson.Unmarshal(v, &existing); err != nil {
					continue // a corrupt value cannot hold a valid claim
				}
				if existing.GetEndpoint().GetAddress() == addr {
					holder = string(k)
					return ErrEndpointHeld
				}
			}
		}
		return bkt.Put([]byte(reg.GetProject()), b)
	})
	return holder, err
}

// Delete removes a project's registration in a transaction. Deleting an absent project is a
// no-op.
func (r *Registry) Delete(project string) error {
	return r.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucket).Delete([]byte(project))
	})
}

// Get returns the live registration for a project, if any.
func (r *Registry) Get(project string) (*registryv1.Registration, bool) {
	var reg *registryv1.Registration
	_ = r.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucket).Get([]byte(project))
		if v == nil {
			return nil
		}
		var got registryv1.Registration
		if err := protojson.Unmarshal(v, &got); err != nil {
			return err
		}
		reg = &got
		return nil
	})
	return reg, reg != nil
}

// List returns the live registrations ordered by project name (bbolt iterates keys in byte
// order; the explicit sort makes the guarantee independent of that).
func (r *Registry) List() []*registryv1.Registration {
	var out []*registryv1.Registration
	_ = r.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucket).ForEach(func(_, v []byte) error {
			var got registryv1.Registration
			if err := protojson.Unmarshal(v, &got); err != nil {
				return nil // skip a corrupt value rather than fail the whole listing
			}
			out = append(out, &got)
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].GetProject() < out[j].GetProject() })
	return out
}

// Set returns the live registrations as the contract message.
func (r *Registry) Set() *registryv1.RegistrationSet {
	return &registryv1.RegistrationSet{Registrations: r.List()}
}

// RefreshLease extends, by ttl from now, the lease of the live registration whose identity
// service_name matches name, and returns true if one was refreshed. A heartbeat for a service
// with no registration is ignored: heartbeats REFRESH leases, they do not create
// registrations -- only Upsert (a /register) creates.
//
// The match is on identity.service_name, the key a heartbeat carries (registrations are
// stored by project, so this scans for the matching identity). service_name is fleet-unique
// -- a frood heartbeats under the identity.service_name it registered, and no two froods
// share one -- so at most one registration matches. The loop refreshes every match defensively
// (a duplicate service_name is a misconfiguration, not a state to silently pick a winner in);
// the canonical home for the uniqueness invariant is frood.v1.Identity.service_name, vendored
// here from big-little-mesh.
func (r *Registry) RefreshLease(name string, ttl time.Duration) (bool, error) {
	if name == "" {
		return false, nil
	}
	refreshed := false
	err := r.db.Update(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(bucket)
		// Collect the (key, new-value) pairs during the cursor scan, then Put them after the
		// loop: mutating the bucket while its cursor iterates is bbolt undefined behavior.
		type update struct {
			key, val []byte
		}
		var updates []update
		c := bkt.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var reg registryv1.Registration
			if err := protojson.Unmarshal(v, &reg); err != nil {
				continue
			}
			if reg.GetIdentity().GetServiceName() != name {
				continue
			}
			reg.LeaseExpiresAt = timestamppb.New(time.Now().UTC().Add(ttl))
			b, err := marshal.Marshal(&reg)
			if err != nil {
				return err
			}
			updates = append(updates, update{key: append([]byte(nil), k...), val: b})
		}
		for _, u := range updates {
			if err := bkt.Put(u.key, u.val); err != nil {
				return err
			}
			refreshed = true
		}
		return nil
	})
	return refreshed, err
}

// ExpireDue removes every registration whose lease_expires_at is at or before now, in one
// transaction, and returns the removed registrations so the caller can log them -- expiry is
// visible, never silent. A registration with no lease stamped is treated as expired (it can
// never be refreshed, so it cannot be trusted).
func (r *Registry) ExpireDue(now time.Time) ([]*registryv1.Registration, error) {
	var expired []*registryv1.Registration
	err := r.db.Update(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(bucket)
		var dueKeys [][]byte
		c := bkt.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var reg registryv1.Registration
			if err := protojson.Unmarshal(v, &reg); err != nil {
				continue
			}
			exp := reg.GetLeaseExpiresAt()
			if exp == nil || !exp.AsTime().After(now) {
				dueKeys = append(dueKeys, append([]byte(nil), k...))
				expired = append(expired, &reg)
			}
		}
		for _, k := range dueKeys {
			if err := bkt.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
	return expired, err
}

// ReconcileWarmStart re-stamps every registration loaded from the store with a short grace
// lease (now + grace). Warm-loaded registrations are provisional: a frood that died while
// delightd was down is expired shortly after boot unless its heartbeat refreshes the lease
// within the grace.
func (r *Registry) ReconcileWarmStart(grace time.Duration) error {
	until := timestamppb.New(time.Now().UTC().Add(grace))
	return r.db.Update(func(tx *bbolt.Tx) error {
		bkt := tx.Bucket(bucket)
		// Collect (key, new-value) during the cursor scan, then Put after the loop: mutating
		// the bucket while its cursor iterates is bbolt undefined behavior.
		type update struct {
			key, val []byte
		}
		var updates []update
		c := bkt.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var reg registryv1.Registration
			if err := protojson.Unmarshal(v, &reg); err != nil {
				continue
			}
			reg.LeaseExpiresAt = until
			b, err := marshal.Marshal(&reg)
			if err != nil {
				return err
			}
			updates = append(updates, update{key: append([]byte(nil), k...), val: b})
		}
		for _, u := range updates {
			if err := bkt.Put(u.key, u.val); err != nil {
				return err
			}
		}
		return nil
	})
}
