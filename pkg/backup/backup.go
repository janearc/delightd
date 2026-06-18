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

// CreateCheckpoint traverses the project directory and writes a compressed
// tarball checkpoint. exclude lists project-relative paths to skip on top of the
// built-in skips (VCS dirs, build artifacts). dryRun walks the manifest without
// writing the tar.
func CreateCheckpoint(ctx context.Context, projectName, projectPath, backupRoot string, maxArchives int, exclude []string, dryRun bool) (string, error) {
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
		fileCount := 0
		err := walkCheckpoint(projectPath, exclude, func(relPath string, d os.DirEntry) error {
			if !d.IsDir() {
				fileCount++
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

	if err := createTarGz(projectPath, archivePath, exclude); err != nil {
		os.Remove(archivePath)
		return "", fmt.Errorf("failed to generate tarball: %w", err)
	}

	enforceRotation(archiveDir, maxArchives)

	return archivePath, nil
}

// walkCheckpoint walks sourceDir and invokes fn for every entry that survives the
// built-in and configured skip rules. Centralizing the skip logic keeps the
// dry-run manifest and the real tar in exact agreement about what gets backed up.
func walkCheckpoint(sourceDir string, exclude []string, fn func(relPath string, d os.DirEntry) error) error {
	return filepath.WalkDir(sourceDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		if relPath == "." {
			return nil
		}
		if d.IsDir() {
			if isSkippedDir(d.Name()) || matchesExclude(relPath, exclude) {
				return filepath.SkipDir
			}
			return fn(relPath, d)
		}
		if isSkippedFile(d.Name()) || matchesExclude(relPath, exclude) {
			return nil
		}
		return fn(relPath, d)
	})
}

func createTarGz(sourceDir, destFile string, exclude []string) error {
	file, err := os.Create(destFile)
	if err != nil {
		return err
	}
	defer file.Close()

	gw := gzip.NewWriter(file)
	defer gw.Close()

	tw := tar.NewWriter(gw)
	defer tw.Close()

	return walkCheckpoint(sourceDir, exclude, func(relPath string, d os.DirEntry) error {
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
		if d.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(filepath.Join(sourceDir, relPath))
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
}

// isSkippedDir and isSkippedFile are the built-in skips applied to every project.
func isSkippedDir(name string) bool {
	switch name {
	case ".git", ".venv", "node_modules", "__pycache__":
		return true
	}
	return false
}

func isSkippedFile(name string) bool {
	switch filepath.Ext(name) {
	case ".o", ".so", ".pyc":
		return true
	}
	return false
}

// matchesExclude reports whether relPath should be excluded. An entry matches
// either as a project-relative path (the entry, or a parent of relPath) or as a
// bare directory/file name at any depth. The name form is what "exclude the
// model dirs" means in practice: comfyui keeps its weights at
// src/ComfyUI/models, not at the project root, so a config of "models" must
// catch it wherever it sits.
func matchesExclude(relPath string, exclude []string) bool {
	base := filepath.Base(relPath)
	for _, e := range exclude {
		e = strings.Trim(e, "/")
		if e == "" {
			continue
		}
		if relPath == e || strings.HasPrefix(relPath, e+"/") || base == e {
			return true
		}
	}
	return false
}

// enforceRotation keeps at most maxArchives checkpoints in archiveDir, deleting
// the oldest. maxArchives <= 0 means unlimited: keep everything. This is the safe
// reading of an unset config -- a missing rotation must never delete the
// checkpoint that was just written.
func enforceRotation(archiveDir string, maxArchives int) {
	if maxArchives <= 0 {
		return
	}

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
