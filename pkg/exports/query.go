package exports

import (
	"os"
	"path/filepath"
	"strings"
)

// HasBashFragment reports whether the daemon manages at least one generated
// docker shim (bash wrapper) for the given project. Sync writes wrappers to
// <stateDir>/<project>/<bin>.sh; their presence on disk is the source of truth.
func (e *Engine) HasBashFragment(project string) bool {
	entries, err := os.ReadDir(filepath.Join(e.stateDir, project))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sh") {
			return true
		}
	}
	return false
}
