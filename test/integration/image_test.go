//go:build integration

// This is the image half of delightd's end-to-end suite: it builds the real container
// image, runs it against a synthetic config, and drives the *full documented control
// surface* (docs/api.md, via verifyControlSurface) against the containerized daemon --
// the same drive daemon_test.go runs against the bare binary. Containerization
// fundamentally changes how delightd is deployed, so the container earns 100% of the
// API, not a /health smoke check.
//
// It also proves the config bake resolves inside the image (docker cp + readlink),
// which the mounted synthetic config deliberately shadows for the API drive.
//
// The one surface not green here is /readyz's apiserver_reachable: the client-go probe
// has no kubeconfig or apiserver route in this test setup (docs/kubernetes-access.md),
// so the drive expects it red and flips to green when a kubeconfig + route are wired.
//
// Gated behind the integration build tag and skipped when docker is unavailable.
package integration

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestContainerImageComesUp(t *testing.T) {
	requireDocker(t)

	const sha = "itest0000"
	tag := "delightd:itest-" + sha

	build := exec.Command("docker", "build", "--build-arg", "GIT_SHA="+sha, "-t", tag, repoRoot())
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build failed: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })

	// The commit-stamped bake must resolve inside the image. scratch has no shell, so
	// copy /etc/delightd out and inspect it.
	assertBakedConfig(t, tag, sha)

	// A synthetic environment mounted into the container: two fake managed repos and a
	// delight.yaml pointing at the in-container roots. Same two projects the bare-binary
	// e2e uses, so the shared control-surface drive asserts identically against both.
	// This config mount shadows the baked /etc/delightd, which is fine -- the bake is
	// verified above, and here we want a controlled roster to drive the API against.
	// Fixtures must live under $HOME: colima shares /Users with its VM, but not
	// /var/folders (t.TempDir) or /tmp, so a bind mount of those lands empty in the
	// container. The container runs as root (no --user, below) to sidestep the
	// macOS->colima virtiofs uid mapping; perms are opened so it can read the repos and
	// config and write /var.
	fixRoot := hostSharedTempDir(t)
	monitorRoot := filepath.Join(fixRoot, "work")
	configRoot := filepath.Join(fixRoot, "etc")
	daemonRoot := filepath.Join(fixRoot, "var")
	for _, d := range []string{monitorRoot, configRoot, daemonRoot} {
		if err := os.MkdirAll(d, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	initFakeRepo(t, filepath.Join(monitorRoot, "fake-clean"), false)
	initFakeRepo(t, filepath.Join(monitorRoot, "fake-dirty"), true)

	// A mocked Ollama backend the containerized daemon must discover. Bound to 0.0.0.0
	// so the container reaches it via host.docker.internal (mapped to host-gateway on
	// the run below).
	mockPort, mockModel := startMockOllama(t)
	cfg := fmt.Sprintf(`system:
  monitor_root: /work
  daemon_root: /var
  backups_root: /var/backups
  config_root: /etc/delightd
  daemon:
    control_port: 8088
  agent_skills:
    enabled: false
  llm_discovery:
    providers:
      - name: "mock"
        type: "ollama"
        url: "http://host.docker.internal:%d"
projects:
  - name: "fake-clean"
    path: /work/fake-clean
    essential: true
    deploy:
      kind: "kube"
      deployment: "fake-clean-agg"
    backup:
      check_interval: "1s"
  - name: "fake-dirty"
    path: /work/fake-dirty
    essential: false
    backup:
      check_interval: "1s"
`, mockPort)
	if err := os.WriteFile(filepath.Join(configRoot, "delight.yaml"), []byte(cfg), 0o644); err != nil {
		t.Fatalf("writing synthetic delight.yaml: %v", err)
	}
	// The container runs as root, so it reads the repos/config at their default perms;
	// the roots were created 0777 so it can write /var. Deliberately no recursive chmod
	// of the repos -- flipping the git files' exec bit makes go-git see a dirty tree.

	name := "delightd-itest-" + sha
	_ = exec.Command("docker", "rm", "-f", name).Run()
	port := freePort(t)
	run := exec.Command("docker", "run", "-d", "--name", name,
		"--add-host", "host.docker.internal:host-gateway", // so discovery can reach the mock backend
		"-e", "DELIGHT_MONITOR_ROOT=/work",
		"-e", "DELIGHT_DAEMON_ROOT=/var",
		"-e", "DELIGHT_BACKUPS_ROOT=/var/backups",
		"-e", "DELIGHT_CONFIG_ROOT=/etc/delightd",
		"-v", monitorRoot+":/work:ro",
		"-v", configRoot+":/etc/delightd:ro",
		"-v", daemonRoot+":/var:rw",
		"-p", fmt.Sprintf("127.0.0.1:%d:8088", port),
		tag, "--immediate")
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run failed: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHealth(t, base, 30*time.Second)

	// The full documented control surface, against the containerized daemon.
	verifyControlSurface(t, base, surfaceExpect{
		cleanPath:          "/work/fake-clean",
		dirtyPath:          "/work/fake-dirty",
		apiserverReachable: false,     // no kubeconfig/route wired in this test setup yet
		wantModel:          mockModel, // /discovery/llms must report the mocked backend
	})
	if t.Failed() {
		t.Logf("container logs:\n%s", containerLogs(name))
	}
}

// requireDocker skips the test when docker is not usable, so the integration lap still
// runs on machines without it.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH; skipping image integration test")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker not available (daemon not reachable); skipping image integration test")
	}
}

// assertBakedConfig copies /etc/delightd out of the image and checks the commit-stamped
// bake resolves: the kube symlink points at kube-<sha>, and the manifests + roster are
// present through it.
func assertBakedConfig(t *testing.T, tag, sha string) {
	t.Helper()
	out, err := exec.Command("docker", "create", tag).Output()
	if err != nil {
		t.Fatalf("docker create: %v", err)
	}
	id := strings.TrimSpace(string(out))
	defer func() { _ = exec.Command("docker", "rm", id).Run() }()

	dst := t.TempDir()
	if out, err := exec.Command("docker", "cp", id+":/etc/delightd", dst).CombinedOutput(); err != nil {
		t.Fatalf("docker cp baked config: %v\n%s", err, out)
	}
	etc := filepath.Join(dst, "delightd")

	link, err := os.Readlink(filepath.Join(etc, "kube"))
	if err != nil {
		t.Fatalf("readlink /etc/delightd/kube: %v", err)
	}
	if want := "kube-" + sha; link != want {
		t.Errorf("baked kube symlink = %q, want %q", link, want)
	}
	for _, f := range []string{
		"delight.yaml",
		filepath.Join("kube", "meubilair.yaml"),
		filepath.Join("kube", "kube", "kustomization.yaml"),
	} {
		if _, err := os.Stat(filepath.Join(etc, f)); err != nil {
			t.Errorf("baked config missing %s: %v", f, err)
		}
	}
}

func containerLogs(name string) string {
	out, _ := exec.Command("docker", "logs", name).CombinedOutput()
	return string(out)
}

// startMockOllama stands up a minimal Ollama-compatible backend (just /api/tags) on
// 0.0.0.0 so the container can reach it via host.docker.internal. Returns the port and
// the single model it advertises. This is the "mock out the models" backend the
// containerized daemon's LLM discovery must find.
func startMockOllama(t *testing.T) (int, string) {
	t.Helper()
	const model = "mock-model:latest"
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen for mock ollama: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/tags" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"models":[{"name":%q}]}`, model)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().(*net.TCPAddr).Port, model
}

// hostSharedTempDir makes a temp dir under $HOME, which colima shares with its VM;
// /var/folders (t.TempDir) and /tmp are not shared, so a bind mount of those is empty
// inside the container. Removed at test end.
func hostSharedTempDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home: %v", err)
	}
	cache := filepath.Join(home, ".cache")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatalf("mkdir ~/.cache: %v", err)
	}
	dir, err := os.MkdirTemp(cache, "delightd-itest-")
	if err != nil {
		t.Fatalf("mktemp under $HOME: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
