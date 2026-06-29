package registry

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	froodv1 "delightd/gen/go/frood/v1"
	registryv1 "delightd/gen/go/registry/v1"
)

// leased builds a registration with an identity service_name (= project) and an explicit
// lease expiry, for the lease tests.
func leased(project, addr string, exp time.Time) *registryv1.Registration {
	return &registryv1.Registration{
		Project:        project,
		Identity:       &froodv1.Identity{ServiceName: project, Project: project},
		Endpoint:       &registryv1.Endpoint{Address: addr},
		LeaseExpiresAt: timestamppb.New(exp),
	}
}

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

// A heartbeat refreshes the lease of the registration whose identity service_name matches.
func TestRefreshLease_ExtendsMatchingService(t *testing.T) {
	r := openTmp(t)
	if err := r.Put(leased("paling", "p:1", time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	ok, err := r.RefreshLease("paling", time.Hour)
	if err != nil || !ok {
		t.Fatalf("refresh: ok=%v err=%v, want true/nil", ok, err)
	}
	g, _ := r.Get("paling")
	if !g.GetLeaseExpiresAt().AsTime().After(time.Now()) {
		t.Fatalf("lease not extended into the future: %v", g.GetLeaseExpiresAt().AsTime())
	}
}

// A heartbeat for a service with no registration refreshes nothing and creates nothing.
func TestRefreshLease_UnknownServiceIsNoOp(t *testing.T) {
	r := openTmp(t)
	ok, err := r.RefreshLease("ghost", time.Hour)
	if err != nil || ok {
		t.Fatalf("unknown service: ok=%v err=%v, want false/nil", ok, err)
	}
	if _, exists := r.Get("ghost"); exists {
		t.Fatal("a heartbeat must not create a registration")
	}
}

// ExpireDue removes lapsed leases and returns them; live ones stay.
func TestExpireDue_RemovesLapsedKeepsLive(t *testing.T) {
	r := openTmp(t)
	if err := r.Put(leased("dead", "d:1", time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if err := r.Put(leased("live", "l:1", time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	expired, err := r.ExpireDue(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].GetProject() != "dead" {
		t.Fatalf("expected only 'dead' expired, got %v", expired)
	}
	if _, ok := r.Get("dead"); ok {
		t.Fatal("lapsed registration not removed")
	}
	if _, ok := r.Get("live"); !ok {
		t.Fatal("live registration wrongly removed")
	}
}

// Warm-start reconciliation: loaded entries get a short grace; one whose heartbeat refreshes
// survives, one that never refreshes is expired once the grace lapses.
func TestReconcileWarmStart_GraceThenExpiry(t *testing.T) {
	r := openTmp(t)
	// stale leases, as if written before a delightd bounce.
	if err := r.Put(leased("survivor", "s:1", time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := r.Put(leased("phantom", "p:1", time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := r.ReconcileWarmStart(50 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// survivor's heartbeat refreshes it to a long lease; phantom never heartbeats.
	if ok, _ := r.RefreshLease("survivor", time.Hour); !ok {
		t.Fatal("survivor refresh failed")
	}
	time.Sleep(80 * time.Millisecond) // let the grace lapse
	expired, err := r.ExpireDue(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(expired) != 1 || expired[0].GetProject() != "phantom" {
		t.Fatalf("expected only 'phantom' expired after grace, got %v", expired)
	}
	if _, ok := r.Get("survivor"); !ok {
		t.Fatal("survivor should have survived via its heartbeat refresh")
	}
}
