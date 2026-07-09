package skills

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	if !strings.Contains(content, "curl -s -X GET \"http://test\"") {
		t.Errorf("missing http handler")
	}
	// A read (GET) carries no bearer.
	if strings.Contains(content, "_authcfg); curl -s -K \"$cfg\" -X GET") {
		t.Errorf("a read must not carry the control-port bearer")
	}
	// A {name} path param becomes a positional shell arg, and a mutating verb carries the
	// bearer via a one-shot curl config (_authcfg), never on curl's argv.
	if !strings.Contains(content, "cfg=$(_authcfg); curl -s -K \"$cfg\" -X POST \"http://localhost:8088/furnish/$1/up\"") {
		t.Errorf("mutating http call missing bearer/positional mapping")
	}
	if !strings.Contains(content, "_authcfg()") || !strings.Contains(content, "Authorization: Bearer") {
		t.Errorf("generated CLI missing the bearer helper")
	}
	if !strings.Contains(content, "exec /bin/dump -v") {
		t.Errorf("missing command handler")
	}
	if !strings.Contains(content, "cfg=$(_authcfg); curl -s -K \"$cfg\" -X POST \"http://localhost:8088/projects/$1/backup\"") {
		t.Errorf("missing internal backup handler with bearer")
	}
	if !strings.Contains(content, "delight example check_health") {
		t.Errorf("missing usage generation")
	}

	// The generated script must be valid bash -- it now carries the _authcfg helper.
	if out, err := exec.Command("bash", "-n", cliPath).CombinedOutput(); err != nil {
		t.Fatalf("generated CLI is not valid bash: %v\n%s", err, out)
	}
}
