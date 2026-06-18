package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	arcPath, err := CreateCheckpoint(ctx, "test-proj", sourceDir, backupRoot, 5, nil, true)
	if err != nil {
		t.Fatalf("dry run error: %v", err)
	}
	if _, err := os.Stat(arcPath); !os.IsNotExist(err) {
		t.Errorf("archive should not exist after dry run")
	}

	// Test real run
	arcPath, err = CreateCheckpoint(ctx, "test-proj", sourceDir, backupRoot, 5, nil, false)
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

// TestEnforceRotation_KeepAllWhenUnset guards the footgun: maxArchives <= 0 must
// mean "keep everything", never "purge everything". A missing rotation config
// previously deleted the checkpoint that was just written.
func TestEnforceRotation_KeepAllWhenUnset(t *testing.T) {
	tmpDir := t.TempDir()
	for i := 0; i < 5; i++ {
		fPath := filepath.Join(tmpDir, string(rune('a'+i))+".tgz")
		os.WriteFile(fPath, []byte("data"), 0644)
	}

	enforceRotation(tmpDir, 0)

	entries, _ := os.ReadDir(tmpDir)
	if len(entries) != 5 {
		t.Errorf("maxArchives=0 must keep all archives, got %d of 5", len(entries))
	}
}

// TestCreateCheckpoint_Excludes verifies a configured exclude keeps a directory
// (e.g. model weights) out of the checkpoint while code is still captured.
func TestCreateCheckpoint_Excludes(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "source")
	backupRoot := filepath.Join(tmpDir, "backups")

	os.MkdirAll(filepath.Join(sourceDir, "models"), 0755)
	os.WriteFile(filepath.Join(sourceDir, "app.py"), []byte("code"), 0644)
	os.WriteFile(filepath.Join(sourceDir, "models", "huge.safetensors"), []byte("weights"), 0644)

	arcPath, err := CreateCheckpoint(ctx, "comfy", sourceDir, backupRoot, 5, []string{"models"}, false)
	if err != nil {
		t.Fatalf("checkpoint error: %v", err)
	}

	names := tarEntryNames(t, arcPath)
	if !contains(names, "app.py") {
		t.Errorf("expected app.py in archive, got %v", names)
	}
	for _, n := range names {
		if n == "models" || n == "models/huge.safetensors" {
			t.Errorf("excluded path %q leaked into the archive", n)
		}
	}
}

// TestCreateCheckpoint_ExcludesNested guards the comfyui case: weights live at a
// nested path (src/ComfyUI/models), so a bare "models" exclude must catch them
// at depth, not only at the project root.
func TestCreateCheckpoint_ExcludesNested(t *testing.T) {
	ctx := context.Background()
	tmpDir := t.TempDir()
	sourceDir := filepath.Join(tmpDir, "source")
	backupRoot := filepath.Join(tmpDir, "backups")

	os.MkdirAll(filepath.Join(sourceDir, "src", "ComfyUI", "models", "unet"), 0755)
	os.WriteFile(filepath.Join(sourceDir, "src", "ComfyUI", "main.py"), []byte("code"), 0644)
	os.WriteFile(filepath.Join(sourceDir, "src", "ComfyUI", "models", "unet", "huge.safetensors"), []byte("weights"), 0644)

	arcPath, err := CreateCheckpoint(ctx, "comfy", sourceDir, backupRoot, 5, []string{"models"}, false)
	if err != nil {
		t.Fatalf("checkpoint error: %v", err)
	}

	names := tarEntryNames(t, arcPath)
	if !contains(names, "src/ComfyUI/main.py") {
		t.Errorf("expected nested code file in archive, got %v", names)
	}
	for _, n := range names {
		if strings.Contains(n, "models") {
			t.Errorf("nested model path %q leaked into the archive", n)
		}
	}
}

func tarEntryNames(t *testing.T, arcPath string) []string {
	t.Helper()
	f, err := os.Open(arcPath)
	if err != nil {
		t.Fatalf("open archive: %v", err)
	}
	defer f.Close()
	gw, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip: %v", err)
	}
	defer gw.Close()

	var names []string
	tw := tar.NewReader(gw)
	for {
		header, err := tw.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("tar read: %v", err)
		}
		names = append(names, header.Name)
	}
	return names
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
