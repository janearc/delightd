package exports

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScanProject(t *testing.T) {
	tmpDir := t.TempDir()

	// Create bin dir with a mock executable
	binDir := filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("failed to create bin dir: %v", err)
	}

	execFile := filepath.Join(binDir, "mock-cli")
	if err := os.WriteFile(execFile, []byte("#!/bin/bash\necho ok"), 0755); err != nil {
		t.Fatalf("failed to write mock executable: %v", err)
	}

	nonExecFile := filepath.Join(binDir, "data.txt")
	if err := os.WriteFile(nonExecFile, []byte("data"), 0644); err != nil {
		t.Fatalf("failed to write mock data file: %v", err)
	}

	exports, err := ScanProject(tmpDir)
	if err != nil {
		t.Fatalf("ScanProject error: %v", err)
	}

	if len(exports) != 1 {
		t.Fatalf("expected 1 export, got %d", len(exports))
	}

	if exports[0].Bin != "mock-cli" || exports[0].Type != TypeBinary {
		t.Errorf("unexpected export data: %+v", exports[0])
	}
}
