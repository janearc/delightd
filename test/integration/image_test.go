//go:build integration

// This is the image half of delightd's end-to-end suite: it builds the real
// container image, runs it, and proves the *containerized* daemon starts and serves
// its control surface -- the repeatable form of the manual "build it and curl it"
// check. The daemon_test.go case proves the bare binary works; this proves the image
// does. Containerization fundamentally changes how delightd is deployed, so it does
// not get to rest on a one-off manual check.
//
// It asserts what the image guarantees today: the config bake resolves inside the
// image, the container boots and loads that baked config, GET /health answers 200, and
// GET /readyz reports roots_readable green. It deliberately does NOT yet assert
// kubectl_reachable -- that check goes green once delightd talks to the cluster via
// client-go (docs/kubernetes-access.md), and this test grows that assertion when that
// lands, up to "the container actually drives k3s."
//
// Gated behind the integration build tag and skipped when docker is unavailable, so
// the default `go test` lap never pays the image build cost.
package integration

import (
	"encoding/json"
	"fmt"
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

	// Build the image. GIT_SHA is required by the Dockerfile's bake assertion.
	build := exec.Command("docker", "build", "--build-arg", "GIT_SHA="+sha, "-t", tag, repoRoot())
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build failed: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })

	// The baked config must resolve inside the image: /etc/delightd/kube -> kube-<sha>,
	// with the manifests and delight.yaml present. scratch has no shell, so copy the
	// tree out and inspect it on the host.
	assertBakedConfig(t, tag, sha)

	// Run the container against throwaway roots so the test can never touch real ~/var.
	name := "delightd-itest-" + sha
	_ = exec.Command("docker", "rm", "-f", name).Run()
	port := freePort(t)
	run := exec.Command("docker", "run", "-d", "--name", name,
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"-e", "DELIGHT_MONITOR_ROOT=/work",
		"-e", "DELIGHT_DAEMON_ROOT=/var",
		"-e", "DELIGHT_CONFIG_ROOT=/etc/delightd",
		"-v", t.TempDir()+":/work:ro",
		"-v", t.TempDir()+":/var:rw",
		"-p", fmt.Sprintf("127.0.0.1:%d:8088", port),
		tag)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("docker run failed: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHealth(t, base, 30*time.Second)

	// /health: the daemon booted and loaded the baked config.
	var health struct {
		Status   string `json:"status"`
		Degraded bool   `json:"degraded"`
	}
	getJSON(t, base+"/health", &health)
	if health.Status != "ok" {
		t.Errorf("/health status = %q, want ok\ncontainer logs:\n%s", health.Status, containerLogs(name))
	}

	// /readyz: roots_readable green means the mounts resolved. It returns 503 right now
	// because kubectl_reachable is red until client-go lands, so read the body
	// regardless of status and assert the roots check -- the piece that stays stable
	// across the client-go work (getJSON demands 200, which /readyz is not yet).
	var ready struct {
		Checks []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
		} `json:"checks"`
	}
	res, err := http.Get(base + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	if err := json.NewDecoder(res.Body).Decode(&ready); err != nil {
		res.Body.Close()
		t.Fatalf("decode /readyz body: %v", err)
	}
	res.Body.Close()
	var rootsFound, rootsOK bool
	for _, c := range ready.Checks {
		if c.Name == "roots_readable" {
			rootsFound, rootsOK = true, c.OK
		}
	}
	if !rootsFound {
		t.Fatalf("/readyz missing roots_readable check: %+v", ready.Checks)
	}
	if !rootsOK {
		t.Errorf("/readyz roots_readable = false, want true (mounts should resolve)")
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
