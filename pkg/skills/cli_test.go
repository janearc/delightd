package skills

import (
	"os"
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
	// A {name} path param becomes a positional shell arg.
	if !strings.Contains(content, "curl -s -X POST \"http://localhost:8088/furnish/$1/up\"") {
		t.Errorf("http path param not mapped to a positional arg")
	}
	if !strings.Contains(content, "exec /bin/dump -v") {
		t.Errorf("missing command handler")
	}
	if !strings.Contains(content, "curl -s -X POST \"http://localhost:8088/projects/$1/backup\"") {
		t.Errorf("missing internal backup handler")
	}
	if !strings.Contains(content, "delight example check_health") {
		t.Errorf("missing usage generation")
	}
}
