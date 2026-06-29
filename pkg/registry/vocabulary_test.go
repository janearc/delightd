package registry

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// chargedVocabulary guards the de-charge of the registry surface: the renamed-away
// RegisterRefused identifier, the "refus..." register, and "reap" (the lease lifecycle is
// named expire/expiry, never reap) MUST NOT creep into this package. If this fails, use the
// neutral register instead (expire / not-completed / NotRegistered).
var chargedVocabulary = regexp.MustCompile(`RegisterRefused|\brefus|\breap`)

// TestNoChargedRegistryVocabulary scans this package's Go source (go test runs with the
// package directory as the working directory) and fails on any charged hit. This guard file
// is skipped, since it necessarily names the pattern it forbids.
func TestNoChargedRegistryVocabulary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f == "vocabulary_test.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if chargedVocabulary.MatchString(line) {
				t.Errorf("%s:%d charged/renamed-away vocabulary: %q", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
