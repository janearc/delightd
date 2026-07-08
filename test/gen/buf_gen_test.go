// Package gen guards codegen determinism: the container image builds Go bindings
// from buf.gen.go.yaml (a Go-only template, no Rust plugin), while dev generates the
// whole set from buf.gen.yaml. The two MUST agree on how Go is generated -- the same
// managed block and the same protoc-gen-go plugin stanza -- or the image bakes Go code
// that differs from what dev builds and reviews. This test fails loudly when they drift
// (delightd #106).
package gen

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// bufGen is the slice of a buf generate template this test compares: the managed block
// (package retargeting) and the plugin list. Only the Go-relevant parts are asserted.
type bufGen struct {
	Managed map[string]any   `yaml:"managed"`
	Plugins []map[string]any `yaml:"plugins"`
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from test dir")
		}
		dir = parent
	}
}

func parseBufGen(t *testing.T, root, name string) bufGen {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var g bufGen
	if err := yaml.Unmarshal(b, &g); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return g
}

// goPlugin returns the protoc-gen-go plugin stanza, failing if it is absent.
func goPlugin(t *testing.T, g bufGen, name string) map[string]any {
	t.Helper()
	for _, p := range g.Plugins {
		if p["local"] == "protoc-gen-go" {
			return p
		}
	}
	t.Fatalf("%s: no protoc-gen-go plugin stanza", name)
	return nil
}

// TestBufGenGoInSyncWithFull asserts the image's Go-only template generates Go exactly
// as the full dev template does: identical managed block and identical protoc-gen-go
// stanza. Editing one plugin's opts, the go_package_prefix, or paths= in buf.gen.yaml
// without mirroring it into buf.gen.go.yaml (or vice versa) is the drift this catches.
func TestBufGenGoInSyncWithFull(t *testing.T) {
	root := repoRoot(t)
	full := parseBufGen(t, root, "buf.gen.yaml")
	goOnly := parseBufGen(t, root, "buf.gen.go.yaml")

	if !reflect.DeepEqual(full.Managed, goOnly.Managed) {
		t.Errorf("managed block differs between buf.gen.yaml and buf.gen.go.yaml:\n full=%#v\n go  =%#v\nkeep them in sync",
			full.Managed, goOnly.Managed)
	}

	fullGo := goPlugin(t, full, "buf.gen.yaml")
	goOnlyGo := goPlugin(t, goOnly, "buf.gen.go.yaml")
	if !reflect.DeepEqual(fullGo, goOnlyGo) {
		t.Errorf("protoc-gen-go stanza differs between buf.gen.yaml and buf.gen.go.yaml:\n full=%#v\n go  =%#v\nkeep them in sync",
			fullGo, goOnlyGo)
	}

	// The image template must NOT run the Rust plugin: its binary is not installed in
	// the build and its output the Go binary never imports. Guarding this keeps the
	// "Go-only" contract from silently regressing into a broken image build.
	for _, p := range goOnly.Plugins {
		if p["local"] == "protoc-gen-rust-serde" {
			t.Error("buf.gen.go.yaml must not run protoc-gen-rust-serde (the image has no such plugin)")
		}
	}
}
