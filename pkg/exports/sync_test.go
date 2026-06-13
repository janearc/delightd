package exports

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestEngineDryRun(t *testing.T) {
	tmpDir := t.TempDir()

	e := &Engine{
		registryPath: filepath.Join(tmpDir, "registry.yaml"),
		varBinDir:    filepath.Join(tmpDir, "bin"),
		stateDir:     filepath.Join(tmpDir, "state"),
		archiveDir:   filepath.Join(tmpDir, "archive"),
		workDir:      filepath.Join(tmpDir, "work"),
	}

	// Create a dummy known project
	err := os.MkdirAll(filepath.Join(tmpDir, "work", "paling", "bin"), 0755)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}

	// Dry run shouldn't write to disk
	err = e.Sync(context.Background(), []string{"paling"}, true)
	if err != nil {
		t.Fatalf("Sync dry-run error: %v", err)
	}

	if _, err := os.Stat(e.varBinDir); !os.IsNotExist(err) {
		t.Errorf("expected varBinDir to not exist in dry run")
	}
}

func TestEngineSyncReal(t *testing.T) {
	tmpDir := t.TempDir()

	e := &Engine{
		registryPath: filepath.Join(tmpDir, "registry.yaml"),
		varBinDir:    filepath.Join(tmpDir, "bin"),
		stateDir:     filepath.Join(tmpDir, "state"),
		archiveDir:   filepath.Join(tmpDir, "archive"),
		workDir:      filepath.Join(tmpDir, "work"),
	}

	// Create registry
	registryYaml := `
projects:
  - name: "odysseus"
    exports:
      - bin: "test-cli"
        type: "docker-run"
        image: "test-img"
        workdir: "/test"
`
	os.WriteFile(e.registryPath, []byte(registryYaml), 0644)

	// Create a dummy project in workDir with a bin
	err := os.MkdirAll(filepath.Join(e.workDir, "odysseus", "bin"), 0755)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	os.WriteFile(filepath.Join(e.workDir, "odysseus", "bin", "native-tool"), []byte("#!/bin/sh"), 0755)

	// Create an orphan symlink to test archiving
	os.MkdirAll(e.varBinDir, 0755)
	os.Symlink(filepath.Join(e.workDir, "old-tool"), filepath.Join(e.varBinDir, "orphan-tool"))

	// Run Sync
	err = e.Sync(context.Background(), []string{"odysseus"}, false)
	if err != nil {
		t.Fatalf("Sync error: %v", err)
	}

	// Check if varBinDir was populated
	if _, err := os.Stat(filepath.Join(e.varBinDir, "test-cli")); os.IsNotExist(err) {
		t.Errorf("expected test-cli symlink")
	}
	if _, err := os.Stat(filepath.Join(e.varBinDir, "native-tool")); os.IsNotExist(err) {
		t.Errorf("expected native-tool symlink")
	}

	// Check if orphan was archived
	if _, err := os.Stat(filepath.Join(e.varBinDir, "orphan-tool")); !os.IsNotExist(err) {
		t.Errorf("expected orphan-tool to be removed from bin dir")
	}

	// Check if state.json was dumped
	if _, err := os.Stat(filepath.Join(e.stateDir, "state.json")); os.IsNotExist(err) {
		t.Errorf("expected state.json to be created")
	}
}

func TestNewEngine(t *testing.T) {
	// Test env var overrides
	os.Setenv("DELIGHT_EXPORTS_BIN", "/test/var/bin")
	defer os.Unsetenv("DELIGHT_EXPORTS_BIN")

	e := NewEngine("/test/work")
	if e.workDir != "/test/work" {
		t.Errorf("expected workDir /test/work, got %s", e.workDir)
	}
	if e.varBinDir != "/test/var/bin" {
		t.Errorf("expected varBinDir /test/var/bin from env, got %s", e.varBinDir)
	}
}
