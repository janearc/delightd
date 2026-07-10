package gen

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// protobufModuleRe extracts the pinned version of google.golang.org/protobuf out of
// go.mod's require block -- the one place a protobuf bump has to touch; ci.yml derives
// its own protoc-gen-go install from this same line, so it tracks automatically.
var protobufModuleRe = regexp.MustCompile(`(?m)^\s*google\.golang\.org/protobuf\s+(v\S+)`)

// dockerfileGenGoRe extracts the protoc-gen-go pin the image's builder stage installs.
// See the comment above that RUN line in the Dockerfile for why it is pinned to the
// same version as the protobuf runtime rather than @latest.
var dockerfileGenGoRe = regexp.MustCompile(`protoc-gen-go@(v\S+)`)

// TestGeneratorPinMatchesRuntime asserts the Dockerfile's protoc-gen-go pin equals
// go.mod's google.golang.org/protobuf version (delightd #106). protoc-gen-go and the
// protobuf runtime are the same module; pinning them to different versions desyncs
// generated code from what the runtime expects, and every other check in this repo
// (gofmt, vet, build, the buf.gen.*.yaml template comparison in buf_gen_test.go) stays
// green through that desync -- none of them look at the version number. ci.yml cannot
// drift on its own because it parses the pin out of go.mod directly at install time;
// this test is what catches the Dockerfile's independent, hand-bumped copy falling
// behind a go.mod bump.
func TestGeneratorPinMatchesRuntime(t *testing.T) {
	root := repoRoot(t)

	goMod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	m := protobufModuleRe.FindSubmatch(goMod)
	if m == nil {
		t.Fatal("go.mod: no google.golang.org/protobuf requirement found")
	}
	runtimeVersion := string(m[1])

	dockerfile, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	d := dockerfileGenGoRe.FindSubmatch(dockerfile)
	if d == nil {
		t.Fatal("Dockerfile: no protoc-gen-go@vX.Y.Z pin found")
	}
	generatorPin := string(d[1])

	if generatorPin != runtimeVersion {
		t.Errorf("Dockerfile pins protoc-gen-go@%s but go.mod runs google.golang.org/protobuf %s -- "+
			"bump both together, or generated code can desync from the runtime it links against",
			generatorPin, runtimeVersion)
	}
}
