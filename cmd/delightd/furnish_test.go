package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeAggregator lays down a minimal kube/ fixture: an aggregator declaring the given
// pieces. The piece directories themselves are not created -- furnish trusts the
// declaration, which is exactly the production contract.
func writeAggregator(t *testing.T, pieces ...string) string {
	t.Helper()
	dir := t.TempDir()
	body := "resources:\n"
	for _, p := range pieces {
		body += "  - " + p + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "kustomization.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return dir
}

// fakeCluster is the test-side cluster seam: it records the pieces up/down touch and
// plays back canned health, so the verbs are exercised without a client-go client.
type fakeCluster struct {
	applied   []string
	removed   []string
	items     []kubeItem
	healthErr error
}

func (f *fakeCluster) apply(_ context.Context, dir string) error {
	f.applied = append(f.applied, dir)
	return nil
}
func (f *fakeCluster) remove(_ context.Context, dir string) error {
	f.removed = append(f.removed, dir)
	return nil
}
func (f *fakeCluster) health(_ context.Context, _ string) ([]kubeItem, error) {
	return f.items, f.healthErr
}

// item builds a kubeItem for the health tests without repeating the nested-struct
// boilerplate.
func item(kind, name string, replicas *int32, ready int32) kubeItem {
	var it kubeItem
	it.Kind = kind
	it.Metadata.Name = name
	it.Spec.Replicas = replicas
	it.Status.ReadyReplicas = ready
	return it
}

// execFurnish runs the furnish command tree against a cluster seam, silencing the JSON
// printJSON writes to stdout so test output stays readable.
func execFurnish(t *testing.T, cl cluster, args ...string) error {
	t.Helper()
	old := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	os.Stdout = devnull
	defer func() { os.Stdout = old; devnull.Close() }()

	cmd := newFurnishCmd(cl)
	cmd.SetArgs(args)
	return cmd.Execute()
}

func TestPiecesReadsTheDeclaration(t *testing.T) {
	dir := writeAggregator(t, "alpha", "beta")
	got, err := pieces(dir)
	if err != nil {
		t.Fatalf("pieces: %v", err)
	}
	if want := []string{"alpha", "beta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("pieces = %v, want %v", got, want)
	}
	if _, err := pieces(t.TempDir()); err == nil {
		t.Error("pieces on a dir with no aggregator: want error, got nil")
	}
	if _, err := pieces(writeAggregator(t)); err == nil {
		t.Error("pieces on an empty declaration: want error, got nil")
	}
}

func TestUpAppliesTheDeclaredPiece(t *testing.T) {
	dir := writeAggregator(t, "surrealdb")
	cl := &fakeCluster{}
	if err := execFurnish(t, cl, "--kube", dir, "up", "surrealdb"); err != nil {
		t.Fatalf("up: %v", err)
	}
	want := filepath.Join(dir, "surrealdb")
	if len(cl.applied) != 1 || cl.applied[0] != want {
		t.Errorf("applied = %v, want [%v]", cl.applied, want)
	}
}

func TestDownRemovesTheDeclaredPiece(t *testing.T) {
	dir := writeAggregator(t, "surrealdb")
	cl := &fakeCluster{}
	if err := execFurnish(t, cl, "--kube", dir, "down", "surrealdb"); err != nil {
		t.Fatalf("down: %v", err)
	}
	want := filepath.Join(dir, "surrealdb")
	if len(cl.removed) != 1 || cl.removed[0] != want {
		t.Errorf("removed = %v, want [%v]", cl.removed, want)
	}
}

func TestUnknownPieceIsRefusedBeforeCluster(t *testing.T) {
	dir := writeAggregator(t, "delightd")
	cl := &fakeCluster{}
	if err := execFurnish(t, cl, "--kube", dir, "up", "ghost"); err == nil {
		t.Fatal("up ghost: want error, got nil")
	}
	if len(cl.applied) != 0 {
		t.Errorf("cluster was touched for an undeclared piece: %v", cl.applied)
	}
}

func TestHealthLadder(t *testing.T) {
	dir := writeAggregator(t, "delightd")
	one, two := int32(1), int32(2)

	health := func(items ...kubeItem) cluster { return &fakeCluster{items: items} }

	// A 1/1 Deployment plus a Service present -> GREEN, exits 0.
	if err := execFurnish(t, health(item("Deployment", "delightd", &one, 1), item("Service", "delightd", nil, 0)),
		"--kube", dir, "health"); err != nil {
		t.Errorf("health on a ready piece: %v", err)
	}
	// A Deployment short of its replicas -> RED, exits non-zero.
	if err := execFurnish(t, health(item("Deployment", "delightd", &two, 0)), "--kube", dir, "health"); err == nil {
		t.Error("health on an unready piece: want non-nil error (RED exits non-zero)")
	}
	// StatefulSets are gated the same as Deployments (the relocated bus/store pieces are
	// StatefulSets), so a not-ready one must go RED.
	if err := execFurnish(t, health(item("StatefulSet", "kafka", &one, 0)), "--kube", dir, "health"); err == nil {
		t.Error("health on an unready StatefulSet: want non-nil error")
	}
	if err := execFurnish(t, health(item("StatefulSet", "kafka", &one, 1)), "--kube", dir, "health"); err != nil {
		t.Errorf("health on a ready StatefulSet: %v", err)
	}
	// A declared-but-absent object is RED, never GREEN-by-existence.
	absent := item("Service", "delightd", nil, 0)
	absent.absent = true
	if err := execFurnish(t, health(absent), "--kube", dir, "health"); err == nil {
		t.Error("health on an absent object: want non-nil error (RED)")
	}
}
