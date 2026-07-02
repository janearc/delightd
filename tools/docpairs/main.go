// docpairs — the doc-pairing gate, mechanical half. A change that touches a
// path paired in .docpairs must touch the paired doc in the same change, or
// this program exits nonzero naming every violation. Pure path logic: it
// proves the doc was TOUCHED, never that it is true (content truth belongs
// to review, which reads the whole document).
//
// Usage (CI, on a pull request):
//
//	go run ./tools/docpairs --base origin/main
//
// The changed set is `git diff --name-only <base>...HEAD`. With no --base the
// program exits 0 loudly: outside a PR there is no base to pair against, and
// a gate that guesses is worse than a gate that says so.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
)

type pair struct {
	glob string
	doc  string
}

func main() {
	base := flag.String("base", "", "git ref to diff against (e.g. origin/main); empty = nothing to check")
	mapPath := flag.String("map", ".docpairs", "path to the pairing map")
	flag.Parse()

	if *base == "" {
		fmt.Println("docpairs: no --base given (not a pull request context); nothing to check")
		return
	}

	pairs, err := loadPairs(*mapPath)
	if err != nil {
		fatal("reading %s: %v", *mapPath, err)
	}
	if len(pairs) == 0 {
		fmt.Printf("docpairs: %s declares no pairings; nothing to check\n", *mapPath)
		return
	}

	out, err := exec.Command("git", "diff", "--name-only", *base+"...HEAD").Output()
	if err != nil {
		fatal("git diff --name-only %s...HEAD: %v", *base, err)
	}
	changed := strings.Fields(string(out))
	if len(changed) == 0 {
		fmt.Println("docpairs: empty diff; nothing to check")
		return
	}

	violations := check(pairs, changed)
	if len(violations) == 0 {
		fmt.Printf("docpairs: ok (%d changed path(s) against %d pairing(s))\n", len(changed), len(pairs))
		return
	}
	for _, v := range violations {
		fmt.Fprintln(os.Stderr, "docpairs: "+v)
	}
	fmt.Fprintln(os.Stderr, "docpairs: a change to a paired path must touch its doc in the same PR (see .docpairs)")
	os.Exit(1)
}

// check is the whole gate, kept pure for the tests: which pairings are
// violated by this changed set?
func check(pairs []pair, changed []string) []string {
	changedSet := make(map[string]bool, len(changed))
	for _, c := range changed {
		changedSet[c] = true
	}
	var violations []string
	for _, p := range pairs {
		var hits []string
		for _, c := range changed {
			// the paired doc itself never triggers its own pairing.
			if c != p.doc && matches(p.glob, c) {
				hits = append(hits, c)
			}
		}
		if len(hits) > 0 && !changedSet[p.doc] {
			violations = append(violations,
				fmt.Sprintf("%s changed without %s (pairing: %q): %s",
					pluralize(hits), p.doc, p.glob, strings.Join(hits, ", ")))
		}
	}
	return violations
}

// matches supports the three shapes .docpairs documents: an exact path, a
// trailing "/**" prefix, and otherwise path.Match's single-segment globbing.
// anything richer should be a deliberate extension, not an accident.
func matches(glob, p string) bool {
	if prefix, ok := strings.CutSuffix(glob, "/**"); ok {
		return p == prefix || strings.HasPrefix(p, prefix+"/")
	}
	ok, err := path.Match(glob, p)
	return err == nil && ok
}

func loadPairs(mapPath string) ([]pair, error) {
	f, err := os.Open(mapPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var pairs []pair
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		glob, doc, ok := strings.Cut(text, "->")
		if !ok {
			return nil, fmt.Errorf("line %d: expected \"<glob> -> <doc>\", got %q", line, text)
		}
		pairs = append(pairs, pair{
			glob: strings.TrimSpace(glob),
			doc:  strings.TrimSpace(doc),
		})
	}
	return pairs, sc.Err()
}

func pluralize(hits []string) string {
	if len(hits) == 1 {
		return "1 path"
	}
	return fmt.Sprintf("%d paths", len(hits))
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "docpairs: "+format+"\n", args...)
	os.Exit(2)
}
