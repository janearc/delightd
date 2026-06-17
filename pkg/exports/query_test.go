package exports

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasBashFragment(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("DELIGHT_EXPORTS_STATE", stateDir)
	e := NewEngine("/work")

	if e.HasBashFragment("paling") {
		t.Error("no fragment expected before any wrapper is written")
	}

	projDir := filepath.Join(stateDir, "paling")
	if err := os.MkdirAll(projDir, 0755); err != nil {
		t.Fatal(err)
	}

	// a non-shell artifact (e.g. state.json) must not count as a fragment
	if err := os.WriteFile(filepath.Join(projDir, "state.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if e.HasBashFragment("paling") {
		t.Error("non-.sh file must not be counted as a bash fragment")
	}

	// a generated wrapper is a fragment
	if err := os.WriteFile(filepath.Join(projDir, "munge.sh"), []byte("#!/usr/bin/env bash\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if !e.HasBashFragment("paling") {
		t.Error("expected a fragment after writing a .sh wrapper")
	}
}

func TestHasBashFragment_ReadErrorIsNotAFragment(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("DELIGHT_EXPORTS_STATE", stateDir)
	e := NewEngine("/work")

	// a regular file where a project directory is expected makes ReadDir fail
	// with a non-NotExist error; we cannot confirm a fragment, so report false
	if err := os.WriteFile(filepath.Join(stateDir, "notadir"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if e.HasBashFragment("notadir") {
		t.Error("a non-directory state path must not be reported as a fragment")
	}
}
