package exports

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Engine handles the synchronization of exports to ~/var/bin and ~/var/runtime
type Engine struct {
	registryPath string
	varBinDir    string
	stateDir     string
	archiveDir   string
	workDir      string
}

func NewEngine(workDir string) *Engine {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = "/"
	}

	getEnv := func(key, fallback string) string {
		if val, ok := os.LookupEnv(key); ok {
			return val
		}
		return fallback
	}

	return &Engine{
		registryPath: getEnv("DELIGHT_EXPORTS_REGISTRY", filepath.Join(home, "etc", "delight-registry.yaml")),
		varBinDir:    getEnv("DELIGHT_EXPORTS_BIN", filepath.Join(home, "var", "bin")),
		stateDir:     getEnv("DELIGHT_EXPORTS_STATE", filepath.Join(home, "var", "runtime", "delightd", "exports")),
		archiveDir:   getEnv("DELIGHT_EXPORTS_ARCHIVE", filepath.Join(home, "var", "archive", "delightd", "exports")),
		workDir:      workDir,
	}
}

func (e *Engine) Sync(ctx context.Context, knownProjects []string, dryRun bool) error {
	var registry Registry
	data, err := os.ReadFile(e.registryPath)
	if err == nil {
		if err := yaml.Unmarshal(data, &registry); err != nil {
			slog.Error("failed to parse delight-registry.yaml", "error", err)
		}
	} else if !os.IsNotExist(err) {
		slog.Error("failed to read delight-registry.yaml", "error", err)
	}

	expected := make(map[string]map[string]ExportDef)

	// Pre-fill from registry
	for _, p := range registry.Projects {
		if expected[p.Name] == nil {
			expected[p.Name] = make(map[string]ExportDef)
		}
		for _, ex := range p.Exports {
			expected[p.Name][ex.Bin] = ex
		}
	}

	// Scan all known projects for sensible defaults
	for _, pName := range knownProjects {
		pPath := filepath.Join(e.workDir, pName)
		if expected[pName] == nil {
			expected[pName] = make(map[string]ExportDef)
		}

		autoExports, _ := ScanProject(pPath)
		for _, ex := range autoExports {
			if _, exists := expected[pName][ex.Bin]; !exists {
				expected[pName][ex.Bin] = ex
			}
		}
	}

	if !dryRun {
		os.MkdirAll(e.varBinDir, 0755)
		os.MkdirAll(e.stateDir, 0755)
	} else {
		slog.Info("[DRY-RUN] Would create state and bin directories", "bin", e.varBinDir, "state", e.stateDir)
	}

	expectedSymlinks := make(map[string]bool)

	for pName, exports := range expected {
		projStateDir := filepath.Join(e.stateDir, pName)

		for binName, def := range exports {
			expectedSymlinks[binName] = true
			binTarget := filepath.Join(e.varBinDir, binName)

			if def.Type == TypeBinary {
				relTarget, err := filepath.Rel(e.varBinDir, def.Target)
				if err != nil {
					relTarget = def.Target
				}
				e.ensureSymlink(relTarget, binTarget, dryRun)
				continue
			}

			if !dryRun {
				os.MkdirAll(projStateDir, 0755)
			}

			wrapperPath := filepath.Join(projStateDir, binName+".sh")
			wrapperCode, err := GenerateWrapper(def)
			if err != nil {
				slog.Error("failed to generate wrapper", "bin", binName, "error", err)
				continue
			}

			if !dryRun {
				if err := os.WriteFile(wrapperPath, []byte(wrapperCode), 0755); err != nil {
					slog.Error("failed to write wrapper script", "path", wrapperPath, "error", err)
					continue
				}
			} else {
				slog.Info("[DRY-RUN] Would write docker shim", "path", wrapperPath)
			}

			relWrapperPath, err := filepath.Rel(e.varBinDir, wrapperPath)
			if err != nil {
				relWrapperPath = wrapperPath
			}
			e.ensureSymlink(relWrapperPath, binTarget, dryRun)
		}
	}

	e.archiveOrphans(expectedSymlinks, dryRun)
	e.dumpState(expected, dryRun)

	return nil
}

func (e *Engine) ensureSymlink(target, link string, dryRun bool) {
	targetPath, err := os.Readlink(link)
	if err == nil && targetPath == target {
		return // already correct
	}

	if dryRun {
		slog.Info("[DRY-RUN] Would create/update symlink", "link", link, "target", target)
		return
	}

	if err == nil || !os.IsNotExist(err) {
		os.Remove(link)
	}

	if err := os.Symlink(target, link); err != nil {
		slog.Error("failed to create symlink", "link", link, "target", target, "error", err)
	} else {
		slog.Debug("created symlink", "link", link, "target", target)
	}
}

func (e *Engine) archiveOrphans(expected map[string]bool, dryRun bool) {
	entries, err := os.ReadDir(e.varBinDir)
	if err != nil {
		return
	}

	// Note: We only check symlinks in ~/var/bin that point into ~/work or ~/var/runtime/delightd.
	// We do not want to archive standard system bins if this was /usr/local/bin.
	for _, entry := range entries {
		binName := entry.Name()
		if !expected[binName] {
			linkPath := filepath.Join(e.varBinDir, binName)
			target, err := os.Readlink(linkPath)
			if err != nil {
				continue
			}

			// Safely identify delightd-managed links to archive
			isWrapper := strings.Contains(target, "delightd/exports")
			isNative := strings.Contains(target, "/work/")

			if !isWrapper && !isNative {
				continue
			}

			ts := time.Now().Format("20060102150405")
			archDir := filepath.Join(e.archiveDir, ts)

			if dryRun {
				slog.Info("[DRY-RUN] Would archive orphaned export", "bin", binName, "archive", archDir)
				continue
			}

			os.MkdirAll(archDir, 0755)

			if isWrapper {
				os.Rename(target, filepath.Join(archDir, binName+".sh"))
			}
			os.Rename(linkPath, filepath.Join(archDir, binName))
			slog.Info("archived orphaned export", "bin", binName, "archive", archDir)
		}
	}
}

func (e *Engine) dumpState(state map[string]map[string]ExportDef, dryRun bool) {
	if dryRun {
		slog.Info("[DRY-RUN] Would dump state to state.json")
		return
	}
	stateFile := filepath.Join(e.stateDir, "state.json")
	data, _ := json.MarshalIndent(state, "", "  ")
	os.WriteFile(stateFile, data, 0644)
}
