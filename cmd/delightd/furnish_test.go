package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// execFurnish runs the furnish command tree with stdout silenced (the relayed JSON would
// clutter test output) and returns the command error, which carries the exit semantics.
func execFurnish(t *testing.T, args ...string) error {
	t.Helper()
	old := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	os.Stdout = devnull
	defer func() { os.Stdout = old; devnull.Close() }()

	cmd := furnishCmd()
	cmd.SetArgs(args)
	return cmd.Execute()
}

// daemonStub stands in for the control port, recording the method+path it was hit with and
// answering with a fixed status/body.
func daemonStub(t *testing.T, status int, body string) (addr string, seen *struct{ method, path string }) {
	t.Helper()
	seen = &struct{ method, path string }{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.method, seen.path = r.Method, r.URL.Path
		w.WriteHeader(status)
		fmt.Fprintln(w, body)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), seen
}

func TestFurnishCLI_ListHitsPieces(t *testing.T) {
	addr, seen := daemonStub(t, http.StatusOK, `{"pieces":["delightd"]}`)
	if err := execFurnish(t, "--control", addr, "list"); err != nil {
		t.Fatalf("list: %v", err)
	}
	if seen.method != http.MethodGet || seen.path != "/furnish/pieces" {
		t.Errorf("list hit %s %s, want GET /furnish/pieces", seen.method, seen.path)
	}
}

func TestFurnishCLI_UpAndDownPost(t *testing.T) {
	addr, seen := daemonStub(t, http.StatusOK, `{"applied":true}`)
	if err := execFurnish(t, "--control", addr, "up", "surrealdb"); err != nil {
		t.Fatalf("up: %v", err)
	}
	if seen.method != http.MethodPost || seen.path != "/furnish/surrealdb/up" {
		t.Errorf("up hit %s %s, want POST /furnish/surrealdb/up", seen.method, seen.path)
	}
	if err := execFurnish(t, "--control", addr, "down", "surrealdb"); err != nil {
		t.Fatalf("down: %v", err)
	}
	if seen.path != "/furnish/surrealdb/down" {
		t.Errorf("down hit %s, want /furnish/surrealdb/down", seen.path)
	}
}

func TestFurnishCLI_HealthPiecePath(t *testing.T) {
	addr, seen := daemonStub(t, http.StatusOK, `{"healthy":true}`)
	if err := execFurnish(t, "--control", addr, "health", "kafka"); err != nil {
		t.Fatalf("health: %v", err)
	}
	if seen.path != "/furnish/health/kafka" {
		t.Errorf("health kafka hit %s, want /furnish/health/kafka", seen.path)
	}
}

func TestFurnishCLI_UnhealthyExitsNonZero(t *testing.T) {
	// A 503 (a RED or INDETERMINATE piece) must surface as a non-zero exit, so the
	// wrapper or an agent gates on it -- the body is still relayed for the operator.
	addr, _ := daemonStub(t, http.StatusServiceUnavailable, `{"healthy":false}`)
	if err := execFurnish(t, "--control", addr, "health"); err == nil {
		t.Error("health against a 503 daemon: want non-nil error (non-zero exit)")
	}
}

func TestFurnishCLI_DaemonDownIsNamed(t *testing.T) {
	// Nothing listening: the error names the likely cause rather than a bare dial error.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := strings.TrimPrefix(srv.URL, "http://")
	srv.Close()
	err := execFurnish(t, "--control", addr, "list")
	if err == nil || !strings.Contains(err.Error(), "delightd") {
		t.Errorf("daemon-down error should name delightd: %v", err)
	}
}
