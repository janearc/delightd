//go:build e2e

// Package registration proves the magpie->delightd registration seam end to end:
// a REAL delightd (built from this tree) accepts a registration sent by magpie's
// REAL client (register_once, via uv in the magpie checkout), and the result is
// observed on the delightd side via GET /registrations — service A talks to
// service B and the product comes out. That observable, cross-service behavior
// is the test that counts for this seam; unit tests around it are hygiene.
//
// What is real: the delightd binary, every gate in handleRegister (roster
// membership, identity consistency, contract verification over live HTTP,
// the /health reachability guarantee), and magpie's client code path.
//
// What is stubbed, stated plainly:
//   - the schema registry: a minimal HTTP stub that answers the version-list
//     probe for any subject. The verification CODE PATH runs for real against
//     a live server; the registry's CONTENT belongs to kafka-svc (the bus
//     layer), which this proof deliberately does not reach -- the seam under
//     test ends at delightd's registry, not at the broker.
//   - magpie's /health endpoint: this test listens on magpie's behalf. The
//     daemon under test here is delightd; launching magpie's full watch daemon
//     would add its filesystem side effects without strengthening the seam
//     assertion. The registration CLIENT is the real magpie code, never mocked.
//
// Local-first by design: expects the magpie checkout at ~/work/magpie and uv
// on PATH. Not wired into CI (recorded follow-up); run with:
//
//	go test -tags e2e ./tests/e2e/registration/ -v
package registration

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// the two contracts magpie declares it emits (magpie/register.py); the seam
// assertion checks both arrive in delightd's live registry intact.
var wantSubjects = []string{
	"observability.v1.ServiceHealthHeartbeat",
	"bento.v1.BentoLifecycleEvent",
}

func TestMagpieRegistersWithDelightd(t *testing.T) {
	repoRoot, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	magpieDir := filepath.Join(home, "work", "magpie")
	if _, err := os.Stat(magpieDir); err != nil {
		t.Fatalf("magpie checkout required at %s (local-first harness): %v", magpieDir, err)
	}

	// stub schema registry: 200 the version list for any subject so the
	// fail-closed verification path completes against a live HTTP surface.
	sr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/subjects/") && strings.HasSuffix(r.URL.Path, "/versions") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "[1]")
			return
		}
		http.NotFound(w, r)
	}))
	defer sr.Close()

	// this test answers magpie's /health guarantee: delightd will dial the
	// registered endpoint before accepting it.
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	}))
	defer health.Close()
	healthAddr := strings.TrimPrefix(health.URL, "http://")

	// build the daemon from THIS tree: the seam proof is about the code under
	// review, not whatever binary happens to be installed.
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "delightd")
	build := exec.Command("go", "build", "-o", bin, "./cmd/delightd")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building delightd: %v\n%s", err, out)
	}

	// isolated config: fake HOME so viper cannot find the operator's real
	// ~/etc/delightd, every root under the temp dir so no real state is
	// touched, an ephemeral control port, and the roster naming magpie.
	ctrlPort := freePort(t)
	fakeHome := filepath.Join(tmp, "home")
	cfgDir := filepath.Join(tmp, "cfg")
	for _, d := range []string{fakeHome, cfgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := fmt.Sprintf(`system:
  daemon:
    control_port: %d
  kafka:
    schema_registry_url: %q
  config_root: %q
  daemon_root: %q
  monitor_root: %q
  backups_root: %q
projects:
  - name: "magpie"
    path: %q
    backup:
      check_interval: "24h"
`, ctrlPort, sr.URL,
		filepath.Join(tmp, "etc"), filepath.Join(tmp, "var"),
		filepath.Join(tmp, "monitor"), filepath.Join(tmp, "backups"),
		magpieDir)
	if err := os.WriteFile(filepath.Join(cfgDir, "delight.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	daemon := exec.Command(bin)
	daemon.Dir = cfgDir // viper's "." config path finds delight.yaml here
	daemon.Env = append(minimalEnv(), "HOME="+fakeHome)
	daemon.Stdout = testWriter{t, "delightd"}
	daemon.Stderr = testWriter{t, "delightd"}
	if err := daemon.Start(); err != nil {
		t.Fatalf("starting delightd: %v", err)
	}
	defer func() {
		_ = daemon.Process.Kill()
		_, _ = daemon.Process.Wait()
	}()

	ctrl := fmt.Sprintf("http://127.0.0.1:%d", ctrlPort)
	waitHealthy(t, ctrl+"/health", 20*time.Second)

	// the act: magpie's REAL registration client, one shot, pointed at the
	// test daemon. uv runs it inside the magpie project's own environment.
	py := fmt.Sprintf(
		`from magpie.register import register_once; r = register_once(endpoint_address=%q, delightd_url=%q); print("registered:", r)`,
		healthAddr, ctrl)
	reg := exec.Command("uv", "run", "python", "-c", py)
	reg.Dir = magpieDir
	if out, err := reg.CombinedOutput(); err != nil {
		t.Fatalf("magpie register_once failed: %v\n%s", err, out)
	} else {
		t.Logf("magpie client: %s", strings.TrimSpace(string(out)))
	}

	// the product, observed on the DELIGHTD side: magpie stands in the live
	// registry with the contracts it declared.
	resp, err := http.Get(ctrl + "/registrations")
	if err != nil {
		t.Fatalf("GET /registrations: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Status        string            `json:"status"`
		Registrations []json.RawMessage `json:"registrations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding /registrations: %v", err)
	}
	if body.Status != "ok" {
		t.Fatalf("registrations status = %q, want ok", body.Status)
	}

	found := false
	for _, raw := range body.Registrations {
		var r struct {
			Project   string `json:"project"`
			Contracts struct {
				Emits []struct {
					Subject string `json:"subject"`
				} `json:"emits"`
			} `json:"contracts"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("decoding registration entry: %v", err)
		}
		if r.Project != "magpie" {
			continue
		}
		found = true
		for _, want := range wantSubjects {
			ok := false
			for _, e := range r.Contracts.Emits {
				if e.Subject == want {
					ok = true
					break
				}
			}
			if !ok {
				t.Errorf("magpie registration missing emit subject %q; raw: %s", want, raw)
			}
		}
	}
	if !found {
		t.Fatalf("magpie not present in the live registry; got %d registration(s)", len(body.Registrations))
	}
}

// freePort grabs an ephemeral port. the tiny bind race between Close and the
// daemon's Listen is acceptable in a local single-user harness.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

// waitHealthy polls until the daemon answers its own /health, or fails the
// test loudly — a daemon that never comes up must not read as a seam failure.
func waitHealthy(t *testing.T, url string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("delightd did not become healthy at %s within %s", url, within)
}

// minimalEnv keeps PATH (go, uv live there) and drops everything else so the
// daemon under test cannot inherit operator state by accident.
func minimalEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH")}
}

// testWriter relays child output into the test log, line-buffered enough for
// debugging a failed run without drowning a green one.
type testWriter struct {
	t    *testing.T
	name string
}

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("[%s] %s", w.name, strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
