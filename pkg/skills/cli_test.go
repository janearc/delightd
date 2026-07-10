package skills

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestGenerateCLIWrapper(t *testing.T) {
	tmpDir := t.TempDir()

	tools := []Tool{
		{
			Name:        "example_check_health",
			Description: "test desc",
			Handler: HandlerDef{
				Type:   "http",
				Method: "GET",
				URL:    "http://test",
			},
		},
		{
			Name: "example_furnish_up",
			Handler: HandlerDef{
				Type:   "http",
				Method: "POST",
				URL:    "http://localhost:8088/furnish/{piece}/up",
			},
		},
		{
			Name: "transparent_dump",
			Handler: HandlerDef{
				Type:    "command",
				Command: "/bin/dump",
				Args:    []string{"-v"},
			},
		},
		{
			Name: "delightd_trigger_backup",
			Handler: HandlerDef{
				Type:   "internal",
				Method: "backup",
			},
		},
	}

	err := GenerateCLIWrapper(tmpDir, tools)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cliPath := filepath.Join(tmpDir, "delight")
	b, err := os.ReadFile(cliPath)
	if err != nil {
		t.Fatalf("failed to read wrapper: %v", err)
	}
	content := string(b)

	// Every http call goes through _call, which prints the body and exits nonzero on a
	// non-2xx -- an agent gates on the exit code (M4, sprints#58).
	if !strings.Contains(content, "_call GET \"http://test\" -d \"$*\"") {
		t.Errorf("missing http handler routed through _call")
	}
	if !strings.Contains(content, "_call() {") {
		t.Errorf("generated CLI missing the _call exit-code helper")
	}
	// A read (GET) carries no bearer.
	if strings.Contains(content, "_authcfg); _call GET") {
		t.Errorf("a read must not carry the control-port bearer")
	}
	// A {name} path param becomes a positional shell arg, and a mutating verb carries the
	// bearer via a one-shot curl config (_authcfg), never on curl's argv. The exit code of
	// _call must survive the cfg cleanup.
	if !strings.Contains(content, "cfg=$(_authcfg); _call POST \"http://localhost:8088/furnish/$1/up\" -K \"$cfg\" -d \"$*\"; rc=$?; rm -f \"$cfg\"; exit $rc") {
		t.Errorf("mutating http call missing bearer/positional/exit-code mapping")
	}
	if !strings.Contains(content, "_authcfg()") || !strings.Contains(content, "Authorization: Bearer") {
		t.Errorf("generated CLI missing the bearer helper")
	}
	if !strings.Contains(content, "exec /bin/dump -v") {
		t.Errorf("missing command handler")
	}
	if !strings.Contains(content, "cfg=$(_authcfg); _call POST \"http://localhost:8088/projects/$1/backup\" -K \"$cfg\"; rc=$?; rm -f \"$cfg\"; exit $rc") {
		t.Errorf("missing internal backup handler with bearer and exit code")
	}
	if !strings.Contains(content, "delight example check_health") {
		t.Errorf("missing usage generation")
	}

	// The generated script must be valid bash -- it now carries the _authcfg helper.
	if out, err := exec.Command("bash", "-n", cliPath).CombinedOutput(); err != nil {
		t.Fatalf("generated CLI is not valid bash: %v\n%s", err, out)
	}
}

// TestGeneratedCLIExitCodes runs the generated wrapper against a live stub daemon and
// asserts the M4 contract (sprints#58): an HTTP failure is a nonzero exit with the body
// still printed, a success is exit zero -- so an agent's tool call gates on health.
func TestGeneratedCLIExitCodes(t *testing.T) {
	var status atomic.Int32 // handler goroutine reads while the test goroutine flips it between requests
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		w.Write([]byte(`{"status":"body-survives"}`))
	}))
	defer srv.Close()

	tmpDir := t.TempDir()
	tools := []Tool{{
		Name:    "example_check_health",
		Handler: HandlerDef{Type: "http", Method: "GET", URL: srv.URL + "/health"},
	}}
	if err := GenerateCLIWrapper(tmpDir, tools); err != nil {
		t.Fatalf("generate: %v", err)
	}
	cliPath := filepath.Join(tmpDir, "delight")

	status.Store(http.StatusServiceUnavailable)
	out, err := exec.Command("bash", cliPath, "example", "check_health").CombinedOutput()
	if err == nil {
		t.Error("5xx from the daemon must exit nonzero")
	}
	if !strings.Contains(string(out), "body-survives") {
		t.Errorf("failure body was not printed: %q", out)
	}

	status.Store(http.StatusOK)
	out, err = exec.Command("bash", cliPath, "example", "check_health").CombinedOutput()
	if err != nil {
		t.Errorf("2xx must exit zero, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "body-survives") {
		t.Errorf("success body was not printed: %q", out)
	}
}
