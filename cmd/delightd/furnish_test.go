package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"delightd/pkg/furnish"
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

// fakeCluster is the test-side cluster seam: it records the pieces up/down touch and plays
// back canned health, so the command verbs are exercised without a client-go client.
type fakeCluster struct {
	applied   []string
	removed   []string
	items     []furnish.Item
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
func (f *fakeCluster) health(_ context.Context, _ string) ([]furnish.Item, error) {
	return f.items, f.healthErr
}

func item(kind, name string, replicas *int32, ready int32) furnish.Item {
	return furnish.Item{Kind: kind, Name: name, Replicas: replicas, ReadyReplicas: ready}
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

// TestHealthExitCode: the command exits zero for a GREEN piece and non-zero for a RED one.
// The health taxonomy itself is tested directly against pkg/furnish; here we only assert
// the command wires PieceHealth's verdict to the exit code.
func TestHealthExitCode(t *testing.T) {
	dir := writeAggregator(t, "delightd")
	one, two := int32(1), int32(2)
	if err := execFurnish(t, &fakeCluster{items: []furnish.Item{item("Deployment", "delightd", &one, 1)}},
		"--kube", dir, "health"); err != nil {
		t.Errorf("health on a ready piece should exit 0: %v", err)
	}
	if err := execFurnish(t, &fakeCluster{items: []furnish.Item{item("Deployment", "delightd", &two, 0)}},
		"--kube", dir, "health"); err == nil {
		t.Error("health on an unready piece should exit non-zero (RED)")
	}
}
