package exports

import (
	"log/slog"
	"os"
	"path/filepath"
)

// ScanProject auto-discovers sensible defaults (like native binaries in bin/) for a project.
func ScanProject(projPath string) ([]ExportDef, error) {
	var exports []ExportDef

	binDir := filepath.Join(projPath, "bin")
	entries, err := os.ReadDir(binDir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("failed to read bin dir", "path", binDir, "error", err)
		}
		return nil, nil // No bin directory, no defaults to provide
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		info, err := entry.Info()
		if err != nil {
			continue
		}

		// Check if file is executable by anyone (Unix convention)
		if info.Mode()&0111 != 0 {
			exports = append(exports, ExportDef{
				Bin:    entry.Name(),
				Type:   TypeBinary,
				Target: filepath.Join(binDir, entry.Name()),
			})
			slog.Debug("auto-discovered native binary", "project", projPath, "bin", entry.Name())
		}
	}

	return exports, nil
}
