package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CreateCheckpoint natively traverses the project directory and creates a compressed tarball.
// Includes a strict dryRun toggle to prevent YOLO writes during testing.
func CreateCheckpoint(ctx context.Context, projectName, projectPath, backupRoot string, maxArchives int, dryRun bool) (string, error) {
	archiveDir := filepath.Join(backupRoot, projectName)
	if !dryRun {
		if err := os.MkdirAll(archiveDir, 0755); err != nil {
			return "", fmt.Errorf("failed to create backup root: %w", err)
		}
	}

	timestamp := time.Now().Format("20060102-150405")
	archivePath := filepath.Join(archiveDir, fmt.Sprintf("%s-%s.tgz", projectName, timestamp))

	if dryRun {
		slog.Info("DRY RUN: evaluating manifest generation", "project", projectName, "target_archive", archivePath)
		// We can still run the WalkDir to prove it works without writing the tar
		fileCount := 0
		err := filepath.WalkDir(projectPath, func(path string, d os.DirEntry, err error) error {
			if err != nil { return err }
			if d.IsDir() {
				name := d.Name()
				if name == ".git" || name == ".venv" || name == "node_modules" || name == "__pycache__" {
					return filepath.SkipDir
				}
			} else {
				ext := filepath.Ext(d.Name())
				if ext != ".o" && ext != ".so" && ext != ".pyc" {
					fileCount++
				}
			}
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("DRY RUN: manifest generation failed: %w", err)
		}
		slog.Info("DRY RUN: success", "project", projectName, "simulated_files_backed_up", fileCount)
		return archivePath, nil
	}

	slog.Info("starting native checkpoint generation", "project", projectName, "archive", archivePath)

	if err := createTarGz(projectPath, archivePath); err != nil {
		os.Remove(archivePath)
		return "", fmt.Errorf("failed to generate tarball: %w", err)
	}

	enforceRotation(archiveDir, maxArchives)

	return archivePath, nil
}

func createTarGz(sourceDir, destFile string) error {
	file, err := os.Create(destFile)
	if err != nil {
		return err
	}
	defer file.Close()

	gw := gzip.NewWriter(file)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	return filepath.WalkDir(sourceDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == ".venv" || name == "node_modules" || name == "__pycache__" {
				return filepath.SkipDir
			}
		}

		if !d.IsDir() {
			ext := filepath.Ext(d.Name())
			if ext == ".o" || ext == ".so" || ext == ".pyc" {
				return nil
			}
		}

		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		header, err := tar.FileInfoHeader(info, info.Name())
		if err != nil {
			return err
		}

		header.Name = filepath.ToSlash(relPath)

		if err := tw.WriteHeader(header); err != nil {
			return err
		}

		if !d.IsDir() && info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()

			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}

		return nil
	})
}

func enforceRotation(archiveDir string, maxArchives int) {
	entries, err := os.ReadDir(archiveDir)
	if err != nil {
		slog.Error("failed to read archive directory for rotation", "error", err)
		return
	}

	var archives []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".tgz") {
			archives = append(archives, entry.Name())
		}
	}

	sort.Strings(archives)

	if len(archives) > maxArchives {
		toDelete := len(archives) - maxArchives
		for i := 0; i < toDelete; i++ {
			oldArc := filepath.Join(archiveDir, archives[i])
			slog.Info("logrotation: purging old checkpoint", "archive", oldArc)
			os.Remove(oldArc)
		}
	}
}
