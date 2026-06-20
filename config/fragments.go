package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// ParseFragment reads a single-project config fragment (one ProjectConfig per
// file) and unmarshals it. It uses an isolated viper so it never disturbs the
// daemon's global config state.
func ParseFragment(path string) (ProjectConfig, error) {
	v := viper.New()
	v.SetConfigFile(path)
	if err := v.ReadInConfig(); err != nil {
		return ProjectConfig{}, fmt.Errorf("reading fragment: %w", err)
	}
	var p ProjectConfig
	if err := v.Unmarshal(&p); err != nil {
		return ProjectConfig{}, fmt.Errorf("parsing fragment: %w", err)
	}
	return p, nil
}

// LoadProjectFragments reads every *.yaml/*.yml drop-in in dir, each a single
// project. It is fail-open per fragment: a missing directory yields nothing (not
// an error), and a malformed or unreadable fragment is skipped with a warning
// rather than aborting -- the blast radius of one bad fragment is that one
// project. Warnings are returned for the caller to fold into its degraded state.
func LoadProjectFragments(dir string) (projects []ProjectConfig, warnings []string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			warnings = append(warnings, fmt.Sprintf("projects.d %q unreadable: %v", dir, err))
		}
		return nil, warnings
	}
	for _, e := range entries {
		if e.IsDir() || !isYAMLName(e.Name()) {
			continue
		}
		p, err := ParseFragment(filepath.Join(dir, e.Name()))
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("fragment %q skipped: %v", e.Name(), err))
			continue
		}
		projects = append(projects, p)
	}
	return projects, warnings
}

func isYAMLName(name string) bool {
	return strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml")
}

// LintResult is the outcome of validating a single fragment. Errors make it
// invalid (a register must refuse it); warnings are advisory -- e.g. a path that
// does not exist on this host, which may be a container path or a not-yet-cloned
// repo, not necessarily a mistake.
type LintResult struct {
	Path     string         `json:"path"`
	Valid    bool           `json:"valid"`
	Project  *ProjectConfig `json:"project,omitempty"`
	Errors   []string       `json:"errors,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
}

// LintFragment validates a fragment without installing it: it must parse and the
// project must be structurally sound (name + path). Path existence and git-repo
// checks are advisory warnings, not errors, because the path may legitimately not
// resolve where lint runs (a container path, or a repo not yet cloned).
func LintFragment(path string) LintResult {
	res := LintResult{Path: path, Valid: true}

	p, err := ParseFragment(path)
	if err != nil {
		res.Valid = false
		res.Errors = append(res.Errors, err.Error())
		return res
	}
	res.Project = &p

	if errs := projectStructuralErrors(p); len(errs) > 0 {
		res.Valid = false
		res.Errors = append(res.Errors, errs...)
	}

	if pp := strings.TrimSpace(p.Path); pp != "" {
		switch fi, err := os.Stat(pp); {
		case err != nil:
			res.Warnings = append(res.Warnings, fmt.Sprintf("path %q not found on this host", pp))
		case !fi.IsDir():
			res.Warnings = append(res.Warnings, fmt.Sprintf("path %q is not a directory", pp))
		default:
			if _, err := os.Stat(filepath.Join(pp, ".git")); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("path %q is not a git repository", pp))
			}
		}
	}
	return res
}

// JSON renders the lint result as indented JSON (the agent-first default).
func (r LintResult) JSON() string {
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}
