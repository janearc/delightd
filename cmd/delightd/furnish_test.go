package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeAggregator lays down a minimal kube/ fixture: an aggregator declaring
// the given pieces. The piece directories themselves are not created --
// furnish trusts the declaration and lets kubectl fail on a missing dir,
// which is exactly the production contract.
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

// recorder is the test-side furnishRunner: it captures every kubectl argv and
// plays back a canned response, the same pattern as the events produce seam.
type recorder struct {
	calls [][]string
	out   []byte
	err   error
}

func (r *recorder) run(_ context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, args)
	return r.out, r.err
}

// execFurnish runs the furnish command tree against a recorder, silencing
// the JSON that printJSON writes to stdout so test output stays readable.
func execFurnish(t *testing.T, rec *recorder, args ...string) error {
	t.Helper()
	old := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	os.Stdout = devnull
	defer func() { os.Stdout = old; devnull.Close() }()

	cmd := newFurnishCmd(rec.run)
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
	rec := &recorder{out: []byte("deployment.apps/surrealdb configured\n")}
	if err := execFurnish(t, rec, "--kube", dir, "up", "surrealdb"); err != nil {
		t.Fatalf("up: %v", err)
	}
	want := []string{"apply", "-k", filepath.Join(dir, "surrealdb")}
	if len(rec.calls) != 1 || !reflect.DeepEqual(rec.calls[0], want) {
		t.Errorf("kubectl argv = %v, want [%v]", rec.calls, want)
	}
}

func TestDownDeletesIgnoringAbsent(t *testing.T) {
	dir := writeAggregator(t, "surrealdb")
	rec := &recorder{out: []byte("")}
	if err := execFurnish(t, rec, "--kube", dir, "down", "surrealdb"); err != nil {
		t.Fatalf("down: %v", err)
	}
	want := []string{"delete", "-k", filepath.Join(dir, "surrealdb"), "--ignore-not-found=true"}
	if len(rec.calls) != 1 || !reflect.DeepEqual(rec.calls[0], want) {
		t.Errorf("kubectl argv = %v, want [%v]", rec.calls, want)
	}
}

func TestUnknownPieceIsRefusedBeforeKubectl(t *testing.T) {
	dir := writeAggregator(t, "delightd")
	rec := &recorder{}
	if err := execFurnish(t, rec, "--kube", dir, "up", "ghost"); err == nil {
		t.Fatal("up ghost: want error, got nil")
	}
	if len(rec.calls) != 0 {
		t.Errorf("kubectl was invoked for an undeclared piece: %v", rec.calls)
	}
}

func TestHealthLadder(t *testing.T) {
	dir := writeAggregator(t, "delightd")

	ready := []byte(`{"items":[
		{"kind":"Deployment","metadata":{"name":"delightd"},"spec":{"replicas":1},"status":{"readyReplicas":1}},
		{"kind":"Service","metadata":{"name":"delightd"}}]}`)
	if err := execFurnish(t, &recorder{out: ready}, "--kube", dir, "health"); err != nil {
		t.Errorf("health on a ready piece: %v", err)
	}

	unready := []byte(`{"items":[
		{"kind":"Deployment","metadata":{"name":"delightd"},"spec":{"replicas":2},"status":{"readyReplicas":0}}]}`)
	if err := execFurnish(t, &recorder{out: unready}, "--kube", dir, "health"); err == nil {
		t.Error("health on an unready piece: want non-nil error (RED exits non-zero)")
	}

	// StatefulSets are gated the same as Deployments: the relocated bus/store
	// pieces (kafka, zookeeper, elasticsearch) are StatefulSets, so a not-ready
	// one must go RED, not GREEN-by-existence.
	stsUnready := []byte(`{"items":[
		{"kind":"StatefulSet","metadata":{"name":"kafka"},"spec":{"replicas":1},"status":{"readyReplicas":0}}]}`)
	if err := execFurnish(t, &recorder{out: stsUnready}, "--kube", dir, "health"); err == nil {
		t.Error("health on an unready StatefulSet: want non-nil error (RED exits non-zero)")
	}

	stsReady := []byte(`{"items":[
		{"kind":"StatefulSet","metadata":{"name":"kafka"},"spec":{"replicas":1},"status":{"readyReplicas":1}}]}`)
	if err := execFurnish(t, &recorder{out: stsReady}, "--kube", dir, "health"); err != nil {
		t.Errorf("health on a ready StatefulSet: %v", err)
	}
}

// TestHealthRedNotAbort proves furnish health reports a piece whose kubectl call
// errors as RED and keeps going, rather than aborting the whole report on the
// first failure -- so one undeployed furniture piece cannot blank out a healthy
// delightd/surrealdb.
func TestHealthRedNotAbort(t *testing.T) {
	dir := writeAggregator(t, "kafka", "surrealdb")
	rec := &recorder{err: errors.New("NotFound")}
	if err := execFurnish(t, rec, "--kube", dir, "health"); err == nil {
		t.Error("health with an erroring piece: want non-nil error (RED exits non-zero)")
	}
	if len(rec.calls) != 2 {
		t.Errorf("health aborted early: queried %d piece(s), want 2 (every declared piece)", len(rec.calls))
	}
}
