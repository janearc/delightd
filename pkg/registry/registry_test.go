package registry

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	citizenv1 "delightd/gen/go/citizen/v1"
	registryv1 "delightd/gen/go/registry/v1"
)

// reg builds a deterministic Registration for round-trip comparison.
func reg(project, addr string) *registryv1.Registration {
	return &registryv1.Registration{
		Project:      project,
		Identity:     &citizenv1.Identity{ServiceName: project, Project: project, Version: "v1"},
		Endpoint:     &registryv1.Endpoint{Scheme: "http", Address: addr},
		RegisteredAt: timestamppb.New(time.Unix(1000, 0).UTC()),
	}
}

func snapPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "registry", "registrations.json")
}

// A cold start (no snapshot file) is an empty registry, not an error.
func TestLoadColdStartEmpty(t *testing.T) {
	r := New(snapPath(t), nil)
	if err := r.Load(); err != nil {
		t.Fatalf("cold-start load: %v", err)
	}
	if got := len(r.List()); got != 0 {
		t.Fatalf("cold start should be empty, got %d", got)
	}
}

// An empty registry round-trips: checkpoint with nothing, reload, still empty.
func TestRoundTripEmpty(t *testing.T) {
	path := snapPath(t)
	if err := New(path, nil).checkpoint(); err != nil {
		t.Fatalf("checkpoint empty: %v", err)
	}
	r := New(path, nil)
	if err := r.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := len(r.List()); got != 0 {
		t.Fatalf("empty round-trip, got %d", got)
	}
}

// Multiple registrations round-trip exactly, ordered by project.
func TestRoundTripMulti(t *testing.T) {
	path := snapPath(t)
	w := New(path, nil)
	// Put out of order; List/snapshot MUST sort by project.
	if err := w.Put(reg("beta", "b:2")); err != nil {
		t.Fatal(err)
	}
	if err := w.Put(reg("alpha", "a:1")); err != nil {
		t.Fatal(err)
	}

	r := New(path, nil)
	if err := r.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := r.List()
	if len(got) != 2 {
		t.Fatalf("got %d registrations, want 2", len(got))
	}
	if !proto.Equal(got[0], reg("alpha", "a:1")) || !proto.Equal(got[1], reg("beta", "b:2")) {
		t.Fatalf("round-trip mismatch: %v", got)
	}
}

// Warm start: a fresh Registry loads the snapshot a prior process wrote, so discovery is
// available immediately.
func TestWarmStartLoad(t *testing.T) {
	path := snapPath(t)
	if err := New(path, nil).Put(reg("paling", "paling.fleet:8090")); err != nil {
		t.Fatal(err)
	}
	r := New(path, nil)
	if err := r.Load(); err != nil {
		t.Fatalf("warm-start load: %v", err)
	}
	g, ok := r.Get("paling")
	if !ok || g.GetEndpoint().GetAddress() != "paling.fleet:8090" {
		t.Fatalf("warm start missing paling: %v ok=%v", g, ok)
	}
}

// The write is atomic: a successful checkpoint leaves no temp file behind, and a partial
// temp file (a simulated mid-write) is never visible to a load -- Load reads only the
// canonical path, so it sees the committed snapshot, never the half-written one.
func TestAtomicWriteNoPartial(t *testing.T) {
	path := snapPath(t)
	if err := New(path, nil).Put(reg("alpha", "a:1")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(path)

	tmps, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(tmps) != 0 {
		t.Fatalf("temp files left after checkpoint: %v", tmps)
	}

	// plant a half-written temp, as if a write had been interrupted before the rename.
	if err := os.WriteFile(filepath.Join(dir, ".registrations-PARTIAL.json.tmp"), []byte("{ half-writt"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(path, nil)
	if err := r.Load(); err != nil {
		t.Fatalf("load must ignore the partial temp, got err: %v", err)
	}
	if _, ok := r.Get("alpha"); !ok {
		t.Fatal("committed snapshot was not loaded")
	}
	if got := len(r.List()); got != 1 {
		t.Fatalf("partial leaked into the load: got %d", got)
	}
}
