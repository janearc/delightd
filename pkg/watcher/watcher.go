package watcher

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/go-git/go-git/v5"
)

// HasChurn evaluates the Git working tree natively without relying on an external OS binary.
// Context is passed down the stack per our tracing standards.
func HasChurn(ctx context.Context, projectPath string) (bool, error) {
	slog.Debug("evaluating git churn", "path", projectPath)

	// Open the repository entirely in memory
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		if err == git.ErrRepositoryNotExists {
			return false, fmt.Errorf("not a git repository: %s", projectPath)
		}
		return false, fmt.Errorf("failed to open git repository: %w", err)
	}

	w, err := r.Worktree()
	if err != nil {
		return false, fmt.Errorf("failed to access worktree: %w", err)
	}

	// Status generates a map of tracked and untracked changes
	status, err := w.Status()
	if err != nil {
		return false, fmt.Errorf("failed to read worktree status: %w", err)
	}

	isClean := status.IsClean()

	if !isClean {
		slog.Info("churn detected in oracle", "path", projectPath, "dirty_files", len(status))
		return true, nil
	}

	return false, nil
}
