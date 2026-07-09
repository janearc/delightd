//go:build integration

// Exercises the `delightd` host wrapper (scripts/delightd) -- the SRE front door. Its
// HTTP-forwarding verbs (status/health/furnish/passthrough) are driven against a stub
// daemon and, for status/health, against the real container image; its lifecycle dispatch
// (start -> colima + docker compose) is driven with those binaries stubbed on PATH. The
// runbook is written against these verbs, so testing them here keeps it from drifting
// into a lie.
package integration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func wrapperPath() string { return filepath.Join(repoRoot(), "scripts", "delightd") }

// runWrapper runs the wrapper with extra env, returning combined output and exit code.
func runWrapper(t *testing.T, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(wrapperPath(), args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("run wrapper %v: %v", args, err)
	return "", -1
}

func TestWrapperSyntax(t *testing.T) {
	if out, err := exec.Command("bash", "-n", wrapperPath()).CombinedOutput(); err != nil {
		t.Fatalf("wrapper is not valid bash: %v\n%s", err, out)
	}
}

// TestWrapperForwarding drives the HTTP verbs against a stub daemon: the wrapper must hit
// the right routes and map the daemon's status onto its own exit code.
func TestWrapperForwarding(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	record := func(r *http.Request) { mu.Lock(); seen = append(seen, r.Method+" "+r.URL.Path); mu.Unlock() }

	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"ready":false}`))
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"status":"ok"}`)) })
	mux.HandleFunc("/furnish/pieces", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"pieces":["delightd"]}`)) })
	mux.HandleFunc("/furnish/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // a RED/INDETERMINATE piece
		w.Write([]byte(`{"healthy":false}`))
	})
	mux.HandleFunc("/furnish/", func(w http.ResponseWriter, r *http.Request) { record(r); w.Write([]byte(`{"ok":true}`)) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	// DELIGHTD_SRC to a throwaway so the wrapper does not source the real checkout's .env.
	env := []string{
		"DELIGHT_CONTROL_ADDR=" + strings.TrimPrefix(srv.URL, "http://"),
		"DELIGHTD_SRC=" + t.TempDir(),
	}

	// status: readyz 503 -> non-zero exit, and it reports the status.
	if out, code := runWrapper(t, env, "status"); code == 0 || !strings.Contains(out, "HTTP 503") {
		t.Errorf("status: code=%d out=%q, want non-zero + 'HTTP 503'", code, out)
	}
	// health (generic passthrough -> GET /health): 200 -> exit 0.
	if out, code := runWrapper(t, env, "health"); code != 0 || !strings.Contains(out, "ok") {
		t.Errorf("health passthrough: code=%d out=%q, want 0 + ok", code, out)
	}
	// furnish list -> GET /furnish/pieces.
	if out, code := runWrapper(t, env, "furnish", "list"); code != 0 || !strings.Contains(out, "delightd") {
		t.Errorf("furnish list: code=%d out=%q", code, out)
	}
	// furnish up <piece> -> POST /furnish/<piece>/up.
	if _, code := runWrapper(t, env, "furnish", "up", "surrealdb"); code != 0 {
		t.Errorf("furnish up: code=%d, want 0", code)
	}
	// furnish health -> 503 -> non-zero exit (gate on it).
	if _, code := runWrapper(t, env, "furnish", "health"); code == 0 {
		t.Error("furnish health against a 503 daemon: want non-zero exit")
	}
	mu.Lock()
	joined := strings.Join(seen, ",")
	mu.Unlock()
	if !strings.Contains(joined, "POST /furnish/surrealdb/up") {
		t.Errorf("furnish up did not hit POST /furnish/surrealdb/up: %s", joined)
	}
}

// TestWrapperCreds drives `delightd creds`: it resolves credentials (op -> token,
// credential-less kubeconfig) and reports, without touching docker/colima. This is the
// isolation point for exercising the op path.
func TestWrapperCreds(t *testing.T) {
	binDir := t.TempDir()
	credsDir := t.TempDir()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	writeStub(t, caFile, "FAKE-CA")
	writeStub(t, filepath.Join(binDir, "op"), "#!/usr/bin/env bash\necho fake-token\n")

	env := []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"DELIGHTD_SRC=" + t.TempDir(), // no real .env sourced
		"DELIGHT_CREDS_DIR=" + credsDir,
		"DELIGHT_CA_SOURCE=" + caFile,
		"DELIGHT_APISERVER=https://k3d-fleet-serverlb:6443",
	}
	out, code := runWrapper(t, env, "creds")
	if code != 0 {
		t.Fatalf("creds: code=%d out=%q", code, out)
	}
	if !strings.Contains(out, "ok: credentials resolve") || !strings.Contains(out, "k3d-fleet-serverlb") {
		t.Errorf("creds summary missing expected lines: %q", out)
	}
	if strings.Contains(out, "fake-token") {
		t.Error("creds printed the token; it must only summarize, never emit the secret")
	}
	if kc, _ := os.ReadFile(filepath.Join(credsDir, "kubeconfig")); !strings.Contains(string(kc), "tokenFile: /run/delightd/token") {
		t.Errorf("kubeconfig not assembled: %s", kc)
	}
}

// TestWrapperSourcesEnv: the wrapper reads per-workstation config from a .env in the
// checkout, so values (here the 1Password reference) need not be exported on every call.
func TestWrapperSourcesEnv(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, ".env"),
		[]byte("DELIGHT_TOKEN_ITEM=op://envtest/delightd/token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	writeStub(t, caFile, "FAKE-CA")
	writeStub(t, filepath.Join(binDir, "op"), "#!/usr/bin/env bash\necho fake-token\n")

	env := []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"DELIGHTD_SRC=" + src,
		"DELIGHT_CREDS_DIR=" + t.TempDir(),
		"DELIGHT_CA_SOURCE=" + caFile,
	}
	out, code := runWrapper(t, env, "creds")
	if code != 0 {
		t.Fatalf("creds: code=%d out=%q", code, out)
	}
	if !strings.Contains(out, "op://envtest/delightd/token") {
		t.Errorf("wrapper did not source .env's DELIGHT_TOKEN_ITEM: %q", out)
	}
}

// TestWrapperStartDispatch stubs colima, docker, git, and op on PATH and asserts `start`
// resolves credentials (reads the token from 1Password, assembles a credential-less
// kubeconfig) and brings the container up through docker compose.
func TestWrapperStartDispatch(t *testing.T) {
	binDir := t.TempDir()
	dockerLog := filepath.Join(t.TempDir(), "docker.log")
	credsDir := t.TempDir()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	writeStub(t, caFile, "FAKE-CA")

	// colima status -> up; git -> a clean stamp; op -> a fake token; docker -> record args.
	writeStub(t, filepath.Join(binDir, "colima"), "#!/usr/bin/env bash\nexit 0\n")
	writeStub(t, filepath.Join(binDir, "git"), `#!/usr/bin/env bash
case "$*" in
  *rev-parse*) echo testsha ;;
  *status*) echo "" ;;
esac
exit 0
`)
	writeStub(t, filepath.Join(binDir, "op"), "#!/usr/bin/env bash\necho fake-token\n")
	writeStub(t, filepath.Join(binDir, "docker"), fmt.Sprintf("#!/usr/bin/env bash\necho \"docker $*\" >> %q\nexit 0\n", dockerLog))

	env := []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"DELIGHTD_SRC=" + t.TempDir(),
		"DELIGHT_CREDS_DIR=" + credsDir,
		"DELIGHT_CA_SOURCE=" + caFile,
	}
	if out, code := runWrapper(t, env, "start"); code != 0 {
		t.Fatalf("start: code=%d out=%q", code, out)
	}

	got, _ := os.ReadFile(dockerLog)
	if !strings.Contains(string(got), "up -d --build delightd") {
		t.Errorf("start did not bring the container up via compose; docker calls:\n%s", got)
	}
	if !strings.Contains(string(got), "network create dev-fleet") {
		t.Errorf("start did not ensure the fleet network; docker calls:\n%s", got)
	}
	// resolve_creds assembled a credential-less kubeconfig pointing at the token file, and
	// laid down the token from op.
	kubeconfig, _ := os.ReadFile(filepath.Join(credsDir, "kubeconfig"))
	if !strings.Contains(string(kubeconfig), "tokenFile: /run/delightd/token") {
		t.Errorf("kubeconfig missing tokenFile:\n%s", kubeconfig)
	}
	if strings.Contains(string(kubeconfig), "fake-token") {
		t.Error("token leaked into the kubeconfig; it must stay in a separate file")
	}
	if tok, _ := os.ReadFile(filepath.Join(credsDir, "token")); strings.TrimSpace(string(tok)) != "fake-token" {
		t.Errorf("token = %q, want fake-token from op", tok)
	}
}

// TestWrapperFurnishHealthDaemonDown: `furnish health` against an unreachable daemon
// must report a clear message and a non-zero exit, not abort silently under set -e
// (the bug in M12 -- cmd_status handles the identical curl failure with `|| echo 000`,
// furnish health didn't).
func TestWrapperFurnishHealthDaemonDown(t *testing.T) {
	env := []string{
		"DELIGHT_CONTROL_ADDR=127.0.0.1:1", // nothing listens here; curl fails fast
		"DELIGHTD_SRC=" + t.TempDir(),
	}
	out, code := runWrapper(t, env, "furnish", "health")
	if code == 0 {
		t.Errorf("furnish health against a down daemon: want non-zero exit, got 0 (%q)", out)
	}
	if !strings.Contains(out, "not reachable") {
		t.Errorf("furnish health against a down daemon: want a clear message, got %q", out)
	}
}

// TestWrapperCredsAtomicWrite: a re-resolve over an existing token must never leave the
// token file empty (M13) -- it must be either the old value or the new one, written via
// temp-file + rename, with no leftover temp file.
func TestWrapperCredsAtomicWrite(t *testing.T) {
	binDir := t.TempDir()
	credsDir := t.TempDir()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	writeStub(t, caFile, "FAKE-CA")
	writeStub(t, filepath.Join(binDir, "op"), "#!/usr/bin/env bash\necho new-token-value-0000000000\n")

	tokenPath := filepath.Join(credsDir, "token")
	if err := os.WriteFile(tokenPath, []byte("old-token-value-0000000000"), 0o600); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"DELIGHTD_SRC=" + t.TempDir(),
		"DELIGHT_CREDS_DIR=" + credsDir,
		"DELIGHT_CA_SOURCE=" + caFile,
	}
	out, code := runWrapper(t, env, "creds")
	if code != 0 {
		t.Fatalf("creds: code=%d out=%q", code, out)
	}

	got, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("token missing after creds: %v", err)
	}
	if len(got) == 0 {
		t.Error("token file is empty after resolve_creds -- write was not atomic")
	}
	if strings.TrimSpace(string(got)) != "new-token-value-0000000000" {
		t.Errorf("token = %q, want the freshly resolved value", got)
	}

	// No leftover temp file from the atomic-write dance, and token permissions stay 0600.
	entries, err := os.ReadDir(credsDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "token" || e.Name() == "ca.crt" || e.Name() == "kubeconfig" {
			continue
		}
		t.Errorf("unexpected leftover in creds dir: %s", e.Name())
	}
	if info, err := os.Stat(tokenPath); err == nil && info.Mode().Perm() != 0o600 {
		t.Errorf("token permissions = %o, want 0600", info.Mode().Perm())
	}
}

// TestWrapperCredsRejectsEmptyToken: `creds` (and by extension `start`) must refuse an
// empty/garbage token from `op read` instead of green-lighting it (M13 sanity check).
func TestWrapperCredsRejectsEmptyToken(t *testing.T) {
	binDir := t.TempDir()
	credsDir := t.TempDir()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	writeStub(t, caFile, "FAKE-CA")
	writeStub(t, filepath.Join(binDir, "op"), "#!/usr/bin/env bash\ntrue\n") // prints nothing

	env := []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"DELIGHTD_SRC=" + t.TempDir(),
		"DELIGHT_CREDS_DIR=" + credsDir,
		"DELIGHT_CA_SOURCE=" + caFile,
	}
	out, code := runWrapper(t, env, "creds")
	if code == 0 {
		t.Errorf("creds with an empty token from op: want non-zero exit, got 0 (%q)", out)
	}
	if _, err := os.Stat(filepath.Join(credsDir, "token")); err == nil {
		t.Error("creds left an empty/garbage token on disk after refusing it")
	}
}

// TestWrapperCredsUnsetsServiceAccountToken: a stray OP_SERVICE_ACCOUNT_TOKEN in the
// environment must not reach `op read` -- it makes op non-interactive and silently
// bypasses the Touch ID gate (M9).
func TestWrapperCredsUnsetsServiceAccountToken(t *testing.T) {
	binDir := t.TempDir()
	credsDir := t.TempDir()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	writeStub(t, caFile, "FAKE-CA")
	writeStub(t, filepath.Join(binDir, "op"),
		"#!/usr/bin/env bash\nif [ -n \"${OP_SERVICE_ACCOUNT_TOKEN:-}\" ]; then echo LEAKED-TOKEN-VALUE-000000; else echo clean-token-value-000000; fi\n")

	env := []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"DELIGHTD_SRC=" + t.TempDir(),
		"DELIGHT_CREDS_DIR=" + credsDir,
		"DELIGHT_CA_SOURCE=" + caFile,
		"OP_SERVICE_ACCOUNT_TOKEN=stray-service-account-token",
	}
	out, code := runWrapper(t, env, "creds")
	if code != 0 {
		t.Fatalf("creds: code=%d out=%q", code, out)
	}
	tok, err := os.ReadFile(filepath.Join(credsDir, "token"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tok), "LEAKED") {
		t.Error("wrapper did not unset OP_SERVICE_ACCOUNT_TOKEN before op read -- silent non-interactive bypass of Touch ID")
	}
}

// TestWrapperStartFailsLoudWithoutRing0: `start` must refuse with an actionable message
// when ring0 (k3d's network, declared external in compose) does not exist yet, instead
// of a cryptic downstream compose failure.
func TestWrapperStartFailsLoudWithoutRing0(t *testing.T) {
	binDir := t.TempDir()
	credsDir := t.TempDir()
	caFile := filepath.Join(t.TempDir(), "ca.crt")
	writeStub(t, caFile, "FAKE-CA")

	writeStub(t, filepath.Join(binDir, "colima"), "#!/usr/bin/env bash\nexit 0\n")
	writeStub(t, filepath.Join(binDir, "git"), `#!/usr/bin/env bash
case "$*" in
  *rev-parse*) echo testsha ;;
  *status*) echo "" ;;
esac
exit 0
`)
	writeStub(t, filepath.Join(binDir, "op"), "#!/usr/bin/env bash\necho fake-token-0000000000\n")
	// dev-fleet network create succeeds; ring0 inspect fails, as it does when k3d has
	// never created it -- the real-world case this guards against.
	writeStub(t, filepath.Join(binDir, "docker"), `#!/usr/bin/env bash
case "$*" in
  "network inspect ring0"*) exit 1 ;;
  *) exit 0 ;;
esac
`)

	env := []string{
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
		"DELIGHTD_SRC=" + t.TempDir(),
		"DELIGHT_CREDS_DIR=" + credsDir,
		"DELIGHT_CA_SOURCE=" + caFile,
	}
	out, code := runWrapper(t, env, "start")
	if code == 0 {
		t.Errorf("start without ring0: want non-zero exit, got 0 (%q)", out)
	}
	if !strings.Contains(out, "ring0") {
		t.Errorf("start without ring0: want an actionable message naming ring0, got %q", out)
	}
}

func writeStub(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestWrapperStatusRealContainer runs status and health against the real container image,
// so the two verbs the runbook leans on are proven against reality, not only a stub.
func TestWrapperStatusRealContainer(t *testing.T) {
	requireDocker(t)
	addr := startMinimalContainer(t)
	env := []string{"DELIGHT_CONTROL_ADDR=" + addr, "DELIGHTD_SRC=" + t.TempDir()}

	// The container has no kubeconfig, so /readyz is red -> status exits non-zero and
	// names the HTTP status. (colima/docker are absent under DELIGHTD_SRC=tmp, reported
	// but not fatal to the readyz-driven exit.)
	out, code := runWrapper(t, env, "status")
	if code == 0 {
		t.Errorf("status against a not-fully-ready container should exit non-zero: %q", out)
	}
	if !strings.Contains(out, "readyz:") {
		t.Errorf("status should report readyz: %q", out)
	}
	// /health is liveness -> the passthrough returns 200 and exits 0.
	if out, code := runWrapper(t, env, "health"); code != 0 || !strings.Contains(out, "\"status\"") {
		t.Errorf("health against the real container: code=%d out=%q", code, out)
	}
}

// startMinimalContainer builds the image and runs delightd with empty roots and no
// projects -- enough to answer /health and /readyz. Returns the control-port address.
func startMinimalContainer(t *testing.T) string {
	t.Helper()
	const sha = "wraptest0"
	tag := "delightd:wraptest-" + sha
	build := exec.Command("docker", "build", "--build-arg", "GIT_SHA="+sha, "-t", tag, repoRoot())
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })

	root := hostSharedTempDir(t)
	for _, d := range []string{"work", "var", "etc"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	cfg := "system:\n  monitor_root: /work\n  daemon_root: /var\n  backups_root: /var/backups\n  config_root: /etc/delightd\n  daemon:\n    control_port: 8088\n  agent_skills:\n    enabled: false\nprojects: []\n"
	if err := os.WriteFile(filepath.Join(root, "etc", "delight.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	name := "delightd-wraptest-" + sha
	_ = exec.Command("docker", "rm", "-f", name).Run()
	port := freePort(t)
	run := exec.Command("docker", "run", "-d", "--name", name,
		"-e", "DELIGHT_MONITOR_ROOT=/work", "-e", "DELIGHT_DAEMON_ROOT=/var",
		"-e", "DELIGHT_BACKUPS_ROOT=/var/backups", "-e", "DELIGHT_CONFIG_ROOT=/etc/delightd",
		"-v", filepath.Join(root, "work")+":/work:ro",
		"-v", filepath.Join(root, "etc")+":/etc/delightd:ro",
		"-v", filepath.Join(root, "var")+":/var:rw",
		"-p", fmt.Sprintf("127.0.0.1:%d:8088", port),
		tag, "--immediate")
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHealth(t, base, 30*time.Second)
	return fmt.Sprintf("127.0.0.1:%d", port)
}
