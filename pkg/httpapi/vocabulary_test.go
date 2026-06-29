package httpapi

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// chargedVocabulary guards the de-charge of the register surface: the renamed-away
// RegisterRefused identifier and the broader "refus..." register MUST NOT creep back into
// this package. If this fails, a renamed-away name or charged wording was reintroduced --
// use the neutral register instead (NotRegistered / not-completed / did-not-complete).
var chargedVocabulary = regexp.MustCompile(`RegisterRefused|\brefus`)

// TestNoChargedRegisterVocabulary scans this package's Go source (go test runs with the
// package directory as the working directory) and fails on any charged hit. This guard
// file is skipped, since it necessarily names the pattern it forbids.
func TestNoChargedRegisterVocabulary(t *testing.T) {
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
