//go:build e2e

// Package enablement proves the state home end to end, across a daemon
// restart: a REAL delightd (built from this tree) serves PUT and GET /state
// over live HTTP, is stopped, is started again on the same daemon root, and
// still knows. "State survives restart" is exactly the claim the unit tests
// cannot carry -- this suite is where it is proven.
//
// Hermetic by construction: the daemon runs with HOME pointing at the test's
// temp tree (config resolves at $HOME/etc/delightd/delight.yaml, state under
// the temp daemon root), any host DELIGHT_* env is stripped, the control port
// is an ephemeral free port, and the daemon runs --dry-run so its backup
// engine never writes. Local-first, not wired into CI (the same recorded
// follow-up as the registration suite); run with:
//
//	go test -tags e2e ./tests/e2e/enablement/ -v
package enablement

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// repoRoot walks up from this test's dir (tests/e2e/enablement).
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(wd)))
}

// buildDaemon compiles the real binary from this tree.
func buildDaemon(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "delightd")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/delightd")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// hermeticEnv is the child environment: the host's, minus every DELIGHT_*
// override, with HOME swapped for the test tree.
func hermeticEnv(home string) []string {
	env := []string{"HOME=" + home}
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "HOME=") || strings.HasPrefix(e, "DELIGHT_") {
			continue
		}
		env = append(env, e)
	}
	return env
}

// writeConfig lays down the hermetic delight.yaml: one project, all roots
// under home, kafka absent (publishers stay nil), the ephemeral port.
func writeConfig(t *testing.T, home string, port int) {
	t.Helper()
	projectPath := filepath.Join(home, "work", "alpha")
	for _, dir := range []string{
		filepath.Join(home, "etc", "delightd"),
		projectPath,
		filepath.Join(home, "var"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	cfg := fmt.Sprintf(`system:
  monitor_root: %q
  daemon_root: %q
  config_root: %q
  daemon:
    control_port: %d
projects:
  - name: alpha
    path: %q
    backup:
      check_interval: "15m"
`, filepath.Join(home, "work"), filepath.Join(home, "var"), filepath.Join(home, "etc"), port, projectPath)
	if err := os.WriteFile(filepath.Join(home, "etc", "delightd", "delight.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// startDaemon launches the binary and waits for /health.
func startDaemon(t *testing.T, bin, home, base string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, "--dry-run")
	cmd.Env = hermeticEnv(home)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return cmd
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	cmd.Process.Kill()
	t.Fatal("daemon never became healthy")
	return nil
}

func stopDaemon(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("sigterm: %v", err)
	}
	cmd.Wait() // exit status is the daemon's business; the test only needs it gone
}

// getState decodes GET /state/{name} into the wire shape.
func getState(t *testing.T, base, name string) (map[string]any, int) {
	t.Helper()
	resp, err := http.Get(base + "/state/" + name)
	if err != nil {
		t.Fatalf("GET /state/%s: %v", name, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body, resp.StatusCode
}

func TestStateSurvivesDaemonRestart(t *testing.T) {
	home := t.TempDir()
	bin := buildDaemon(t, t.TempDir())
	port := freePort(t)
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	writeConfig(t, home, port)

	// First life: absent reads disabled, then a real disable is recorded.
	daemon := startDaemon(t, bin, home, base)
	body, code := getState(t, base, "alpha")
	if code != http.StatusOK || body["state"] != "disabled" || body["recorded"] != false {
		t.Fatalf("fresh state = %d %v, want 200 disabled/unrecorded", code, body)
	}

	req, _ := http.NewRequest(http.MethodPut, base+"/state/alpha",
		strings.NewReader(`{"state":"disabled","reason":"e2e restart proof","actor":"e2e"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT code = %d, want 200", resp.StatusCode)
	}
	stopDaemon(t, daemon)

	// Second life, same daemon root: the record is still there.
	daemon = startDaemon(t, bin, home, base)
	defer stopDaemon(t, daemon)
	body, code = getState(t, base, "alpha")
	if code != http.StatusOK {
		t.Fatalf("post-restart code = %d, want 200", code)
	}
	if body["state"] != "disabled" || body["recorded"] != true || body["reason"] != "e2e restart proof" {
		t.Fatalf("post-restart state = %v; the record did not survive the restart", body)
	}
}
