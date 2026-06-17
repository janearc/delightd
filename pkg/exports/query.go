package exports

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// HasBashFragment reports whether the daemon manages at least one "bash fragment"
// for the given project. A bash fragment is a generated docker shim: a small
// shell wrapper script (<bin>.sh) that Sync writes under <stateDir>/<project>/ so
// a host command transparently proxies into a container. Their presence on disk
// is the source of truth. See the "Bash fragments (docker shims)" section of
// README.md for the full explanation.
//
// A missing project directory is the normal "no fragment" case and returns false
// quietly. Any other read error (permissions, not-a-directory) cannot confirm a
// fragment, so it is logged and reported as false rather than silently swallowed.
func (e *Engine) HasBashFragment(project string) bool {
	entries, err := os.ReadDir(filepath.Join(e.stateDir, project))
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("failed to read exports state dir for introspection",
				"project", project, "error", err)
		}
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sh") {
			return true
		}
	}
	return false
}
