//go:build integration

// Exercises scripts/install.sh -- the documented cold-bring-up entry point. It never
// touches the real network or the operator's real $HOME/var/bin: git/docker/colima are
// stubbed on PATH the same way wrapper_test.go stubs them, and DELIGHTD_BIN_DIR redirects
// the installed symlink into a temp dir.
package integration

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func installPath() string { return filepath.Join(repoRoot(), "scripts", "install.sh") }

// runInstall runs install.sh with a full env replacement (not appended to os.Environ,
// so PATH can be trimmed to simulate a missing prerequisite), returning combined output
// and exit code.
func runInstall(t *testing.T, env []string) (string, int) {
	t.Helper()
	cmd := exec.Command(installPath())
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	}
	t.Fatalf("run install.sh: %v", err)
	return "", -1
}

// pathWithoutTool returns a PATH on which no directory carries an executable named
// `name`, used to simulate a missing prerequisite. A directory that contains the tool
// is not dropped -- on CI runners docker shares /usr/bin with bash and coreutils, so
// dropping it would break /usr/bin/env bash -- it is replaced by a symlink replica of
// itself minus the tool, so everything else still resolves.
func pathWithoutTool(t *testing.T, name string) string {
	t.Helper()
	var kept []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			kept = append(kept, dir)
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s to replicate it without %s: %v", dir, name, err)
		}
		replica := t.TempDir()
		for _, e := range entries {
			if e.Name() == name {
				continue
			}
			if err := os.Symlink(filepath.Join(dir, e.Name()), filepath.Join(replica, e.Name())); err != nil {
				t.Fatalf("symlink %s into replica of %s: %v", e.Name(), dir, err)
			}
		}
		kept = append(kept, replica)
	}
	return strings.Join(kept, string(os.PathListSeparator))
}

func TestInstallSyntax(t *testing.T) {
	if out, err := exec.Command("bash", "-n", installPath()).CombinedOutput(); err != nil {
		t.Fatalf("install.sh is not valid bash: %v\n%s", err, out)
	}
}

// TestInstallChecksPrereqsBeforeClone: a missing prerequisite (docker) must fail
// immediately, before install.sh ever touches git clone/pull -- the reordering fix for
// the minor where the tool check ran after the clone.
func TestInstallChecksPrereqsBeforeClone(t *testing.T) {
	src := filepath.Join(t.TempDir(), "delightd") // absent -> a clone would be attempted
	binDir := t.TempDir()
	cloneLog := filepath.Join(t.TempDir(), "git.log")
	writeStub(t, filepath.Join(binDir, "git"),
		fmt.Sprintf("#!/usr/bin/env bash\necho \"git $*\" >> %q\nexit 0\n", cloneLog))

	env := []string{
		"HOME=" + t.TempDir(),
		"DELIGHTD_SRC=" + src,
		"PATH=" + binDir + ":" + pathWithoutTool(t, "docker"),
	}
	out, code := runInstall(t, env)
	if code == 0 {
		t.Errorf("install without docker: want non-zero exit, got 0 (%q)", out)
	}
	if !strings.Contains(out, "missing required tool: docker") {
		t.Errorf("install without docker: want a clear message, got %q", out)
	}
	if _, err := os.Stat(cloneLog); err == nil {
		t.Error("install.sh ran git before the prerequisite check -- clone was attempted despite missing docker")
	}
}

// TestInstallBootstrapsEnvAndRefuses: a clean checkout with no .env must get one created
// from .env.example and then refuse (B3) -- never build against placeholder mount paths.
func TestInstallBootstrapsEnvAndRefuses(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, ".env.example"), []byte("DELIGHT_MONITOR_ROOT_HOST=/Users/you/work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	writeStub(t, filepath.Join(binDir, "git"), gitStubBody())
	writeStub(t, filepath.Join(binDir, "docker"), "#!/usr/bin/env bash\nexit 0\n")
	writeStub(t, filepath.Join(binDir, "colima"), "#!/usr/bin/env bash\nexit 0\n")

	env := []string{
		"HOME=" + t.TempDir(),
		"DELIGHTD_SRC=" + src,
		"DELIGHTD_BIN_DIR=" + t.TempDir(),
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
	}
	out, code := runInstall(t, env)
	if code == 0 {
		t.Errorf("install with no .env: want non-zero exit, got 0 (%q)", out)
	}
	if !strings.Contains(out, ".env") || !strings.Contains(out, "rerun install.sh") {
		t.Errorf("install with no .env: want an actionable message, got %q", out)
	}

	got, err := os.ReadFile(filepath.Join(src, ".env"))
	if err != nil {
		t.Fatalf(".env was not created from .env.example: %v", err)
	}
	if !strings.Contains(string(got), "DELIGHT_MONITOR_ROOT_HOST") {
		t.Errorf(".env contents = %q, want it copied from .env.example", got)
	}
}

// TestInstallBuildsWhenEnvPresent: with a filled-in .env already in place, install.sh
// sources it and proceeds to build -- the documented entry point must actually reach
// `docker compose build` on a clean machine once .env is set (B3).
func TestInstallBuildsWhenEnvPresent(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// install.sh's final smoke check runs "$BIN_DIR/delightd" --help, and the installed
	// symlink points at $SRC/scripts/delightd -- the fake checkout needs the real wrapper
	// there for that step to resolve.
	if err := os.MkdirAll(filepath.Join(src, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	wrapperBody, err := os.ReadFile(wrapperPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "scripts", "delightd"), wrapperBody, 0o755); err != nil {
		t.Fatal(err)
	}
	envBody := "DELIGHT_MONITOR_ROOT_HOST=" + t.TempDir() + "\n" +
		"DELIGHT_DAEMON_ROOT_HOST=" + t.TempDir() + "\n" +
		"DELIGHT_CREDS_DIR=" + t.TempDir() + "\n"
	if err := os.WriteFile(filepath.Join(src, ".env"), []byte(envBody), 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	dockerLog := filepath.Join(t.TempDir(), "docker.log")
	writeStub(t, filepath.Join(binDir, "git"), gitStubBody())
	writeStub(t, filepath.Join(binDir, "docker"),
		fmt.Sprintf("#!/usr/bin/env bash\necho \"docker $*\" >> %q\nexit 0\n", dockerLog))
	writeStub(t, filepath.Join(binDir, "colima"), "#!/usr/bin/env bash\nexit 0\n")

	binDirDest := t.TempDir()
	env := []string{
		"HOME=" + t.TempDir(),
		"DELIGHTD_SRC=" + src,
		"DELIGHTD_BIN_DIR=" + binDirDest,
		"PATH=" + binDir + ":" + os.Getenv("PATH"),
	}
	out, code := runInstall(t, env)
	if code != 0 {
		t.Fatalf("install with .env present: code=%d out=%q", code, out)
	}

	got, _ := os.ReadFile(dockerLog)
	if !strings.Contains(string(got), "build delightd") {
		t.Errorf("install did not build the image; docker calls:\n%s", got)
	}
	if _, err := os.Lstat(filepath.Join(binDirDest, "delightd")); err != nil {
		t.Errorf("install did not install the wrapper symlink into DELIGHTD_BIN_DIR: %v", err)
	}
}

// gitStubBody handles the git invocations install.sh makes against an existing checkout:
// pull, rev-parse --short HEAD, and status --porcelain.
func gitStubBody() string {
	return `#!/usr/bin/env bash
case "$*" in
  *"pull --ff-only"*) exit 0 ;;
  *"rev-parse --short HEAD"*) echo testsha ;;
  *"status --porcelain"*) echo "" ;;
  *) exit 0 ;;
esac
`
}
