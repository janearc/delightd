// Package registry holds delightd's in-memory set of live citizen registrations -- the
// state the /register broker maintains. It sits ALONGSIDE the static yaml/poll roster and
// does not replace it; nothing in the broker is mandatory yet.
//
// The on-disk snapshot is a WARM-START CACHE, not the source of truth. Authority is the
// live process: the snapshot exists only so a delightd bounce does not blank discovery.
// Entries loaded from it MUST NOT be trusted permanently -- the lease (a later step) reaps
// any that do not renew. The snapshot's form IS the contract (`registry.v1.RegistrationSet`
// as protojson), so the cache cannot drift from the wire shape.
package registry

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"google.golang.org/protobuf/encoding/protojson"

	registryv1 "delightd/gen/go/registry/v1"
)

// marshal is the snapshot/serve encoding: protojson with proto field names, so the on-disk
// and on-wire shapes match the contract exactly.
var marshal = protojson.MarshalOptions{UseProtoNames: true}

// Registry is the live set of citizen registrations, keyed by declared project name -- one
// live registration per project. The mesh is nodes==1 today; a multi-replica registry is a
// future thought, deliberately NOT built here. All access is guarded so handlers and the
// (later) reap sweep can touch it without a data race.
type Registry struct {
	mu        sync.RWMutex
	byProject map[string]*registryv1.Registration
	path      string
	log       *slog.Logger
}

// New builds an empty Registry whose snapshot lives at path. It does not read the snapshot;
// call Load for warm start.
func New(path string, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}
	return &Registry{
		byProject: make(map[string]*registryv1.Registration),
		path:      path,
		log:       log,
	}
}

// Load reads the warm-start snapshot into memory. A missing snapshot is not an error -- a
// cold start is an empty registry. Loaded entries are not trusted permanently (see the
// package doc); for now they are simply made available so discovery works immediately.
func (r *Registry) Load() error {
	b, err := os.ReadFile(r.path)
	if errors.Is(err, fs.ErrNotExist) {
		r.log.Info("registry: no snapshot, cold start", "path", r.path)
		return nil
	}
	if err != nil {
		return err
	}
	var set registryv1.RegistrationSet
	if err := protojson.Unmarshal(b, &set); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, reg := range set.GetRegistrations() {
		r.byProject[reg.GetProject()] = reg
	}
	r.log.Info("registry: warm-started from snapshot", "path", r.path, "count", len(r.byProject))
	return nil
}

// Put records (or replaces) the live registration for its project and checkpoints the
// snapshot. One registration per project: re-Putting a project overwrites it.
func (r *Registry) Put(reg *registryv1.Registration) error {
	r.mu.Lock()
	r.byProject[reg.GetProject()] = reg
	r.mu.Unlock()
	return r.checkpoint()
}

// Delete removes a project's registration (e.g. a reap) and checkpoints. Deleting an absent
// project is a no-op and still checkpoints, so the on-disk form always reflects memory.
func (r *Registry) Delete(project string) error {
	r.mu.Lock()
	delete(r.byProject, project)
	r.mu.Unlock()
	return r.checkpoint()
}

// Get returns the live registration for a project, if any.
func (r *Registry) Get(project string) (*registryv1.Registration, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	reg, ok := r.byProject[project]
	return reg, ok
}

// List returns the live registrations, ordered by project name so the snapshot and the
// GET /registrations response are deterministic.
func (r *Registry) List() []*registryv1.Registration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sortedLocked()
}

// Set returns the live registrations as the contract message -- the form served on the wire
// and persisted on disk.
func (r *Registry) Set() *registryv1.RegistrationSet {
	return &registryv1.RegistrationSet{Registrations: r.List()}
}

// sortedLocked returns the registrations ordered by project. Caller MUST hold the lock.
func (r *Registry) sortedLocked() []*registryv1.Registration {
	out := make([]*registryv1.Registration, 0, len(r.byProject))
	for _, reg := range r.byProject {
		out = append(out, reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetProject() < out[j].GetProject() })
	return out
}

// checkpoint writes the snapshot atomically: a temp file in the same directory, fsync'd,
// then renamed over the target. A reader therefore never observes a partial file -- it sees
// either the previous complete snapshot or the new one, never a half-written one. A failed
// write leaves the target untouched.
func (r *Registry) checkpoint() error {
	r.mu.RLock()
	set := &registryv1.RegistrationSet{Registrations: r.sortedLocked()}
	r.mu.RUnlock()

	b, err := marshal.Marshal(set)
	if err != nil {
		return err
	}

	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".registrations-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// best-effort cleanup if we bail before the rename; after a successful rename the temp
	// name no longer exists, so the remove is a harmless no-op.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, r.path); err != nil {
		return err
	}
	// fsync the directory so the rename itself is durable across a crash.
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
