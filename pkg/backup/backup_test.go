package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateCheckpoint(t *testing.T) {
	ctx := context.Background()

	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "source")
	backupRoot := filepath.Join(tmpDir, "backups")

	os.MkdirAll(sourceDir, 0755)

	// Write some test files
	os.WriteFile(filepath.Join(sourceDir, "test.txt"), []byte("hello world"), 0644)

	// Test dry run
	arcPath, err := CreateCheckpoint(ctx, "test-proj", sourceDir, backupRoot, 5, true)
	if err != nil {
		t.Fatalf("dry run error: %v", err)
	}
	if _, err := os.Stat(arcPath); !os.IsNotExist(err) {
		t.Errorf("archive should not exist after dry run")
	}

	// Test real run
	arcPath, err = CreateCheckpoint(ctx, "test-proj", sourceDir, backupRoot, 5, false)
	if err != nil {
		t.Fatalf("real run error: %v", err)
	}

	if _, err := os.Stat(arcPath); os.IsNotExist(err) {
		t.Errorf("archive should exist after real run")
	}

	// Verify tar contents
	f, err := os.Open(arcPath)
	if err != nil {
		t.Fatalf("failed to open archive: %v", err)
	}
	defer f.Close()

	gw, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("failed to read gzip: %v", err)
	}
	defer gw.Close()

	tw := tar.NewReader(gw)
	found := false
	for {
		header, err := tw.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read error: %v", err)
		}
		if header.Name == "test.txt" {
			found = true
			break
		}
	}

	if !found {
		t.Errorf("expected test.txt in archive")
	}
}

func TestEnforceRotation(t *testing.T) {
	tmpDir := t.TempDir()

	for i := 0; i < 5; i++ {
		fPath := filepath.Join(tmpDir, string(rune('a'+i))+".tgz")
		os.WriteFile(fPath, []byte("data"), 0644)
	}

	enforceRotation(tmpDir, 3)

	entries, _ := os.ReadDir(tmpDir)
	if len(entries) != 3 {
		t.Errorf("expected 3 archives, got %d", len(entries))
	}

	// Check if the oldest ones were deleted. 'a' and 'b' should be gone.
	if entries[0].Name() != "c.tgz" {
		t.Errorf("expected oldest archives to be deleted")
	}
}
