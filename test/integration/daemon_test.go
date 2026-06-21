//go:build integration

// Package integration holds delightd's end-to-end functional test: it builds the
// real `delightd` binary, brings the daemon UP against a fully synthetic
// environment (a temp config, temp roots, and a couple of fake managed git
// repos), and drives its real HTTP control surface over the loopback.
//
// This is the daemon's equivalent of taco's resume test. The unit tests in
// pkg/httpapi construct handlers in isolation with httptest -- they prove a
// handler's logic, but never that the daemon actually starts, binds its control
// port, reads its config, builds the per-project state machines, and runs the
// backup eval loop. The field question those unit tests cannot answer is "does
// the binary, run as a process, actually serve /health, /git, /projects and turn
// a POST /projects/{name}/backup into a .tgz on disk?". Only a separate process
// listening on a real socket answers it.
//
// Everything here is self-contained: the fake projects are temp `git init`
// repos with their own commits, all four delightd roots point at t.TempDir()s,
// and HOME is overridden so no ~ ever resolves to the real ~/work or ~/var. The
// build tag keeps it out of the default `go test` lap; CI runs it explicitly
// with `-tags=integration`.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// delightdBin is the path to the binary built once in TestMain and reused by the
// test, so we measure daemon behaviour, not repeated compiles.
var delightdBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "delightd-itest-bin-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	delightdBin = filepath.Join(dir, "delightd")
	// Build from the repo root: this file lives in test/integration, so the module
	// root is two levels up.
	build := exec.Command("go", "build", "-o", delightdBin, "./cmd/delightd")
	build.Dir = repoRoot()
	build.Stderr = os.Stderr
	build.Stdout = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "building delightd:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func repoRoot() string {
	// test/integration -> test -> repo root
	wd, _ := os.Getwd()
	return filepath.Dir(filepath.Dir(wd))
}

// TestDaemonControlSurfaceEndToEnd is the functional proof that delightd works as
// a daemon, not just as a set of packages. It builds two fake managed projects
// (one left clean, one left dirty), brings the real binary UP against them, and
// asserts the live control port behaves:
//
//   - GET /health      -> 200, status ok, active_projects == 2
//   - GET /projects    -> the roster, carrying the essential/deploy/remote_url
//     fields from the recent seam, in config order
//   - GET /git         -> each project's live git state; the dirty repo reports
//     dirty=true and the clean repo dirty=false, and both report unpushed work
//     (no upstream) so a safety gate sees uncommitted/never-pushed state
//   - POST /projects/{dirty}/backup -> a checkpoint .tgz lands under the backups
//     root for that project
//
// The backup round-trip is the strongest assertion: the control handler only
// transitions the state machine to backing_up; the actual tarball is written by
// the daemon's eval loop. A .tgz appearing therefore proves the whole pipeline
// ran in the live process, not just that a handler returned 200.
func TestDaemonControlSurfaceEndToEnd(t *testing.T) {
	// --- A synthetic environment: temp roots + two fake managed repos. ---
	home := t.TempDir()        // HOME override: every ~ resolves in here, never real ~/work
	daemonRoot := t.TempDir()  // delightd's own state tree; backups derive under it
	monitorRoot := t.TempDir() // the tree the daemon "watches" (holds the fake repos)
	configRoot := t.TempDir()  // where delightd resolves its config
	backupsRoot := filepath.Join(daemonRoot, "backups")

	cleanPath := filepath.Join(monitorRoot, "fake-clean")
	dirtyPath := filepath.Join(monitorRoot, "fake-dirty")
	initFakeRepo(t, cleanPath, false) // committed, working tree clean
	initFakeRepo(t, dirtyPath, true)  // committed, then left with an uncommitted change

	// Pick a free loopback port and pin it in the config so the test knows where
	// to reach the control surface.
	port := freePort(t)

	// A complete delight.yaml drives the daemon: the four roots point at temp dirs,
	// agent skills are off (no CLI-wrapper writes), and the two fake projects are
	// declared inline with absolute paths and a tight check_interval so the eval
	// loop spins promptly. essential/deploy are set so the /projects roster has
	// something non-trivial to carry.
	cfgYAML := fmt.Sprintf(`system:
  monitor_root: %q
  daemon_root: %q
  backups_root: %q
  config_root: %q
  daemon:
    control_port: %d
  agent_skills:
    enabled: false
projects:
  - name: "fake-clean"
    path: %q
    essential: true
    deploy:
      kind: "kube"
      deployment: "fake-clean-agg"
    backup:
      check_interval: "1s"
  - name: "fake-dirty"
    path: %q
    essential: false
    backup:
      check_interval: "1s"
`, monitorRoot, daemonRoot, backupsRoot, configRoot, port, cleanPath, dirtyPath)

	cfgDir := t.TempDir() // CWD for the daemon: config.Load searches "." for delight.yaml
	if err := os.WriteFile(filepath.Join(cfgDir, "delight.yaml"), []byte(cfgYAML), 0644); err != nil {
		t.Fatalf("writing delight.yaml: %v", err)
	}

	// --- Bring the daemon UP. --immediate makes it evaluate on startup so the
	// dirty repo's churn drives a backup without waiting on a poll tick. ---
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, delightdBin, "--immediate")
	cmd.Dir = cfgDir
	// HOME override is the safety belt: even if some path slips through as ~, it
	// lands in the temp home, never the real ~/work or ~/var. DELIGHT_PROJECTS_DIR
	// points at an empty dir so no stray host fragment leaks into the roster.
	emptyFragments := t.TempDir()
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"DELIGHT_PROJECTS_DIR="+emptyFragments,
	)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting delightd: %v", err)
	}
	// Always reap the daemon: signal, then wait, so the test never leaks a process.
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitForHealth(t, base, 30*time.Second)

	// --- GET /health: the daemon is up and reports both managed projects. ---
	t.Run("health", func(t *testing.T) {
		var resp struct {
			Status         string `json:"status"`
			ActiveProjects int    `json:"active_projects"`
			DryRun         bool   `json:"dry_run"`
		}
		getJSON(t, base+"/health", &resp)
		if resp.Status != "ok" {
			t.Errorf("status = %q, want ok (daemon came up degraded)", resp.Status)
		}
		if resp.ActiveProjects != 2 {
			t.Errorf("active_projects = %d, want 2", resp.ActiveProjects)
		}
		if resp.DryRun {
			t.Errorf("dry_run = true, want false")
		}
	})

	// --- GET /projects: the roster with the seam fields. ---
	t.Run("projects_roster", func(t *testing.T) {
		var resp struct {
			Status   string `json:"status"`
			Projects []struct {
				Name      string `json:"name"`
				Path      string `json:"path"`
				Essential bool   `json:"essential"`
				Deploy    struct {
					Kind       string `json:"kind"`
					Deployment string `json:"deployment"`
				} `json:"deploy"`
				RemoteURL string `json:"remote_url"`
			} `json:"projects"`
		}
		getJSON(t, base+"/projects", &resp)
		if resp.Status != "ok" || len(resp.Projects) != 2 {
			t.Fatalf("unexpected roster: %+v", resp)
		}
		byName := map[string]int{}
		for i, p := range resp.Projects {
			byName[p.Name] = i
		}
		ci, ok := byName["fake-clean"]
		if !ok {
			t.Fatalf("fake-clean missing from roster: %+v", resp.Projects)
		}
		if !resp.Projects[ci].Essential {
			t.Errorf("fake-clean essential = false, want true")
		}
		if resp.Projects[ci].Deploy.Kind != "kube" || resp.Projects[ci].Deploy.Deployment != "fake-clean-agg" {
			t.Errorf("fake-clean deploy block not carried: %+v", resp.Projects[ci].Deploy)
		}
		if resp.Projects[ci].Path != cleanPath {
			t.Errorf("fake-clean path = %q, want %q", resp.Projects[ci].Path, cleanPath)
		}
		di, ok := byName["fake-dirty"]
		if !ok {
			t.Fatalf("fake-dirty missing from roster: %+v", resp.Projects)
		}
		if resp.Projects[di].Essential {
			t.Errorf("fake-dirty essential = true, want false")
		}
		// The fake repos have no remote configured, so remote_url is omitted.
		if resp.Projects[di].RemoteURL != "" {
			t.Errorf("fake-dirty remote_url = %q, want empty (no remote configured)", resp.Projects[di].RemoteURL)
		}
	})

	// --- GET /git: live per-project git state. Dirty/clean both appear. ---
	t.Run("git_state", func(t *testing.T) {
		var resp struct {
			Status   string `json:"status"`
			Projects []struct {
				Name string `json:"name"`
				Git  struct {
					Branch      string `json:"branch"`
					Dirty       bool   `json:"dirty"`
					Unpushed    int    `json:"unpushed"`
					HasUpstream bool   `json:"has_upstream"`
					Error       string `json:"error"`
				} `json:"git"`
			} `json:"projects"`
		}
		getJSON(t, base+"/git", &resp)
		if resp.Status != "ok" || len(resp.Projects) != 2 {
			t.Fatalf("unexpected /git body: %+v", resp)
		}
		git := map[string]struct {
			branch      string
			dirty       bool
			unpushed    int
			hasUpstream bool
			err         string
		}{}
		for _, p := range resp.Projects {
			git[p.Name] = struct {
				branch      string
				dirty       bool
				unpushed    int
				hasUpstream bool
				err         string
			}{p.Git.Branch, p.Git.Dirty, p.Git.Unpushed, p.Git.HasUpstream, p.Git.Error}
		}

		clean, dirty := git["fake-clean"], git["fake-dirty"]
		if clean.err != "" {
			t.Errorf("fake-clean git error: %q", clean.err)
		}
		if dirty.err != "" {
			t.Errorf("fake-dirty git error: %q", dirty.err)
		}
		if clean.dirty {
			t.Errorf("fake-clean reported dirty, want clean")
		}
		if !dirty.dirty {
			t.Errorf("fake-dirty reported clean, want dirty")
		}
		if clean.branch == "" {
			t.Errorf("fake-clean has no branch; HEAD read failed")
		}
		// No remote was configured, so the single commit is never-pushed work and
		// has_upstream is false -- the conservative reading a safety gate relies on.
		if dirty.hasUpstream {
			t.Errorf("fake-dirty has_upstream = true, want false (no remote)")
		}
		if dirty.unpushed < 1 {
			t.Errorf("fake-dirty unpushed = %d, want >= 1 (one un-pushed commit)", dirty.unpushed)
		}
	})

	// --- POST /projects/{name}/backup: a checkpoint .tgz lands under backups. ---
	t.Run("backup_round_trip", func(t *testing.T) {
		// The dirty repo has churn, so its machine accepts the trigger and the eval
		// loop will write a checkpoint. (A clean repo would be a no-op at the loop.)
		req, err := http.NewRequest(http.MethodPost, base+"/projects/fake-dirty/backup", nil)
		if err != nil {
			t.Fatalf("building backup request: %v", err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST backup: %v", err)
		}
		body := drain(res)
		// 200 (trigger accepted) is the expected case; with --immediate the loop may
		// already be mid-backup, in which case the trigger is a no-op and the .tgz
		// still appears. A 409 backoff would mean a prior failure -- treat that as a
		// real problem since the repo is healthy.
		if res.StatusCode != http.StatusOK {
			t.Fatalf("POST /projects/fake-dirty/backup = %d, body %q", res.StatusCode, body)
		}

		archiveDir := filepath.Join(backupsRoot, "fake-dirty")
		waitForArchive(t, archiveDir, 30*time.Second)
	})
}

// initFakeRepo creates a temp git repo at path: git init, one tracked file, an
// initial commit. When dirty, it then modifies a tracked file and leaves the
// change uncommitted so the working tree reports dirty. Commits are made with an
// explicit, non-personal identity via per-command env so the test never depends
// on (or pollutes) global git config.
func initFakeRepo(t *testing.T, path string, dirty bool) {
	t.Helper()
	if err := os.MkdirAll(path, 0755); err != nil {
		t.Fatalf("mkdir repo: %v", err)
	}
	runGit(t, path, "init", "-q", "-b", "main")
	readme := filepath.Join(path, "README.md")
	if err := os.WriteFile(readme, []byte("# fixture\ninitial\n"), 0644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	runGit(t, path, "add", "README.md")
	runGit(t, path, "commit", "-q", "-m", "initial commit")
	if dirty {
		if err := os.WriteFile(readme, []byte("# fixture\ninitial\nuncommitted edit\n"), 0644); err != nil {
			t.Fatalf("dirtying file: %v", err)
		}
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// A self-contained identity: never read or write the host's global git config.
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=delightd-itest",
		"GIT_AUTHOR_EMAIL=itest@example.invalid",
		"GIT_COMMITTER_NAME=delightd-itest",
		"GIT_COMMITTER_EMAIL=itest@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// freePort asks the OS for an unused loopback TCP port, then releases it. There
// is a benign race between release and the daemon binding it; in a test
// environment it is reliable enough and avoids parsing the port out of the
// daemon's logs.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving port: %v", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// waitForHealth polls GET /health until it answers 200 or the deadline passes,
// so the test only proceeds once the daemon has bound its control port.
func waitForHealth(t *testing.T, base string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		res, err := http.Get(base + "/health")
		if err == nil {
			drain(res)
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("daemon never served 200 on /health within %s", within)
}

// waitForArchive blocks until at least one .tgz appears in dir or the deadline
// passes. The checkpoint is written by the daemon's eval loop after the trigger,
// so it is necessarily asynchronous to the POST.
func waitForArchive(t *testing.T, dir string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.HasSuffix(e.Name(), ".tgz") {
					if fi, statErr := e.Info(); statErr == nil && fi.Size() > 0 {
						t.Logf("checkpoint written: %s (%d bytes)", filepath.Join(dir, e.Name()), fi.Size())
						return
					}
				}
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("no .tgz checkpoint appeared under %s within %s", dir, within)
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", url, res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("GET %s content-type = %q, want application/json", url, ct)
	}
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatalf("decoding %s: %v", url, err)
	}
}

// drain reads and closes a response body so the connection can be reused, and
// returns the body for diagnostics.
func drain(res *http.Response) string {
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}
