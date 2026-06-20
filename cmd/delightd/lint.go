package main

import (
	"flag"
	"fmt"
	"os"

	"delightd/config"
)

// runLint validates one or more project fragments and prints the result -- JSON by
// default (agent-first), --text for a human. Exit is non-zero if any fragment is
// invalid, so it composes in scripts and as a pre-register check. It is read-only:
// linting never installs or touches the daemon's running config.
func runLint(args []string) int {
	fs := flag.NewFlagSet("lint", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	text := fs.Bool("text", false, "human-readable output (default: JSON)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: delightd lint [--text] <fragment.yaml> [more.yaml ...]")
		return 2
	}

	exit := 0
	for _, path := range fs.Args() {
		res := config.LintFragment(path)
		if *text {
			renderLintText(res)
		} else {
			fmt.Println(res.JSON())
		}
		if !res.Valid {
			exit = 1
		}
	}
	return exit
}

func renderLintText(res config.LintResult) {
	status := "ok"
	if !res.Valid {
		status = "INVALID"
	}
	fmt.Printf("%s: %s\n", res.Path, status)
	for _, e := range res.Errors {
		fmt.Printf("  error:   %s\n", e)
	}
	for _, w := range res.Warnings {
		fmt.Printf("  warning: %s\n", w)
	}
}
