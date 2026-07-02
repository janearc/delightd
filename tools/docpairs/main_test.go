package main

import (
	"strings"
	"testing"
)

var testPairs = []pair{
	{glob: "pkg/httpapi/**", doc: "docs/api.md"},
	{glob: "Taskfile.yml", doc: "docs/operations.md"},
	{glob: "config/**", doc: "docs/operations.md"},
}

func TestPairedChangeWithoutDocFails(t *testing.T) {
	v := check(testPairs, []string{"pkg/httpapi/register.go"})
	if len(v) != 1 || !strings.Contains(v[0], "docs/api.md") {
		t.Fatalf("want one api.md violation, got %v", v)
	}
}

func TestPairedChangeWithDocPasses(t *testing.T) {
	v := check(testPairs, []string{"pkg/httpapi/register.go", "docs/api.md"})
	if len(v) != 0 {
		t.Fatalf("doc touched; want no violations, got %v", v)
	}
}

func TestUnpairedChangePasses(t *testing.T) {
	v := check(testPairs, []string{"pkg/registry/registry.go", "README.md"})
	if len(v) != 0 {
		t.Fatalf("nothing paired changed; want no violations, got %v", v)
	}
}

func TestDocOnlyChangePasses(t *testing.T) {
	// touching the doc alone is fine; the doc never triggers its own pairing.
	v := check(testPairs, []string{"docs/api.md"})
	if len(v) != 0 {
		t.Fatalf("doc-only change must pass, got %v", v)
	}
}

func TestExactPathPairing(t *testing.T) {
	v := check(testPairs, []string{"Taskfile.yml"})
	if len(v) != 1 || !strings.Contains(v[0], "docs/operations.md") {
		t.Fatalf("Taskfile.yml without operations.md must fail, got %v", v)
	}
}

func TestTwoPairingsOneDoc(t *testing.T) {
	// Taskfile and config both pair to operations.md; touching both with the
	// doc satisfies both pairings, without it violates both.
	v := check(testPairs, []string{"Taskfile.yml", "config/config.go"})
	if len(v) != 2 {
		t.Fatalf("want two violations (both pair to operations.md), got %v", v)
	}
	v = check(testPairs, []string{"Taskfile.yml", "config/config.go", "docs/operations.md"})
	if len(v) != 0 {
		t.Fatalf("doc touched; want none, got %v", v)
	}
}

func TestMatcherShapes(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"pkg/httpapi/**", "pkg/httpapi/register.go", true},
		{"pkg/httpapi/**", "pkg/httpapi/deep/nested.go", true},
		{"pkg/httpapi/**", "pkg/httpapi", true},           // the dir itself
		{"pkg/httpapi/**", "pkg/httpapifake/x.go", false}, // prefix must be a segment
		{"Taskfile.yml", "Taskfile.yml", true},
		{"Taskfile.yml", "sub/Taskfile.yml", false},
		{"*.md", "README.md", true},
		{"*.md", "docs/api.md", false}, // single segment only
	}
	for _, c := range cases {
		if got := matches(c.glob, c.path); got != c.want {
			t.Errorf("matches(%q, %q) = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}
