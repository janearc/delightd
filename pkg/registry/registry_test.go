package registry

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	registryv1 "delightd/gen/go/registry/v1"
)

func reg(project, addr string) *registryv1.Registration {
	return &registryv1.Registration{
		Project:  project,
		Endpoint: &registryv1.Endpoint{Scheme: "http", Address: addr},
	}
}

func openTmp(t *testing.T) *Registry {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "registry", "registry.db"), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

// A freshly-opened store is empty.
func TestColdStartEmpty(t *testing.T) {
	if got := len(openTmp(t).List()); got != 0 {
		t.Fatalf("cold start not empty: %d", got)
	}
}

// Put then Get/List; List is ordered by project regardless of insert order.
func TestPutGetListSorted(t *testing.T) {
	r := openTmp(t)
	if err := r.Put(reg("beta", "b:2")); err != nil {
		t.Fatal(err)
	}
	if err := r.Put(reg("alpha", "a:1")); err != nil {
		t.Fatal(err)
	}
	g, ok := r.Get("alpha")
	if !ok || g.GetEndpoint().GetAddress() != "a:1" {
		t.Fatalf("get alpha: %v ok=%v", g, ok)
	}
	list := r.List()
	if len(list) != 2 || list[0].GetProject() != "alpha" || list[1].GetProject() != "beta" {
		t.Fatalf("list not sorted: %v", list)
	}
}

// bbolt persistence is the warm start: a reopen of the same file sees prior writes.
func TestWarmStartAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry", "registry.db")
	r1, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r1.Put(reg("paling", "paling:8090")); err != nil {
		t.Fatal(err)
	}
	if err := r1.Close(); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer r2.Close()
	if g, ok := r2.Get("paling"); !ok || g.GetEndpoint().GetAddress() != "paling:8090" {
		t.Fatalf("warm start lost paling: %v ok=%v", g, ok)
	}
}

func TestDelete(t *testing.T) {
	r := openTmp(t)
	if err := r.Put(reg("x", "x:1")); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete("x"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("x"); ok {
		t.Fatal("delete did not remove")
	}
	if err := r.Delete("absent"); err != nil {
		t.Fatalf("delete of absent project should be a no-op: %v", err)
	}
}

// Upsert is the atomic collision check: a different project on the same endpoint is held;
// the same project re-claiming its own endpoint is idempotent.
func TestUpsertCollisionAndIdempotent(t *testing.T) {
	r := openTmp(t)
	if _, err := r.Upsert(reg("alpha", "shared:9000")); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	holder, err := r.Upsert(reg("beta", "shared:9000"))
	if !errors.Is(err, ErrEndpointHeld) || holder != "alpha" {
		t.Fatalf("collision: holder=%q err=%v, want alpha/ErrEndpointHeld", holder, err)
	}
	if _, ok := r.Get("beta"); ok {
		t.Fatal("beta must not be recorded when its endpoint is held")
	}
	if _, err := r.Upsert(reg("alpha", "shared:9000")); err != nil {
		t.Fatalf("same-project re-upsert should be idempotent: %v", err)
	}
}

// Concurrent writers and readers must not race (run with -race) and the store stays
// consistent.
func TestConcurrentAccess(t *testing.T) {
	r := openTmp(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			p := fmt.Sprintf("p%d", n)
			if err := r.Put(reg(p, fmt.Sprintf("a:%d", n))); err != nil {
				t.Errorf("put: %v", err)
			}
			_, _ = r.Get(p)
			_ = r.List()
			if _, err := r.Upsert(reg(p, fmt.Sprintf("a:%d", n))); err != nil {
				t.Errorf("upsert: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if got := len(r.List()); got != 8 {
		t.Fatalf("want 8 registrations, got %d", got)
	}
}
