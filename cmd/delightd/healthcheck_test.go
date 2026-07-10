package main

import (
	"net/http"
	"os"
	"testing"
)

// execHealthcheck runs the healthcheck command tree with stdout silenced, the same pattern
// execFurnish uses -- healthcheck relays the daemon's /readyz body verbatim (furnishDo), which
// would otherwise clutter test output.
func execHealthcheck(t *testing.T, args ...string) error {
	t.Helper()
	old := os.Stdout
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	os.Stdout = devnull
	defer func() { os.Stdout = old; devnull.Close() }()

	cmd := healthcheckCmd()
	cmd.SetArgs(args)
	return cmd.Execute()
}

func TestHealthcheck_ReadyIsZeroExit(t *testing.T) {
	addr, seen := daemonStub(t, http.StatusOK, `{"ready":true,"checks":[]}`)
	if err := execHealthcheck(t, "--control", addr); err != nil {
		t.Fatalf("healthcheck on a ready daemon: %v", err)
	}
	if seen.method != http.MethodGet || seen.path != "/readyz" {
		t.Errorf("healthcheck hit %s %s, want GET /readyz", seen.method, seen.path)
	}
}

func TestHealthcheck_NotReadyIsNonZeroExit(t *testing.T) {
	addr, _ := daemonStub(t, http.StatusServiceUnavailable, `{"ready":false,"checks":[]}`)
	if err := execHealthcheck(t, "--control", addr); err == nil {
		t.Error("healthcheck on a 503 daemon: want non-nil error (non-zero exit)")
	}
}
