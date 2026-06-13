package watcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
)

func TestHasChurn(t *testing.T) {
	ctx := context.Background()

	// Test with non-existent directory
	tmpDir := t.TempDir()
	churn, err := HasChurn(ctx, filepath.Join(tmpDir, "nonexistent"))
	if err == nil {
		t.Errorf("expected error for nonexistent directory")
	}

	// Test with empty directory (not a git repo)
	churn, err = HasChurn(ctx, tmpDir)
	if err == nil {
		t.Errorf("expected error for non-git directory")
	}

	// Initialize git repo
	r, err := git.PlainInit(tmpDir, false)
	if err != nil {
		t.Fatalf("failed to init git repo: %v", err)
	}

	// Test clean repo
	churn, err = HasChurn(ctx, tmpDir)
	if err != nil {
		t.Fatalf("unexpected error for clean repo: %v", err)
	}
	if churn {
		t.Errorf("expected false for clean repo")
	}

	// Make repo dirty
	dirtyFile := filepath.Join(tmpDir, "dirty.txt")
	os.WriteFile(dirtyFile, []byte("test"), 0644)

	w, _ := r.Worktree()
	w.Add("dirty.txt")

	// Test dirty repo
	churn, err = HasChurn(ctx, tmpDir)
	if err != nil {
		t.Fatalf("unexpected error for dirty repo: %v", err)
	}
	if !churn {
		t.Errorf("expected true for dirty repo")
	}
}
