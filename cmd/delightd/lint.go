package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"delightd/config"
)

// lintCmd validates one or more project fragments without installing them -- JSON
// by default (agent-first), --text for a human. It returns an error (→ non-zero
// exit) if any fragment is invalid, so it composes as a pre-register check. It is
// read-only: linting never installs or touches the daemon's running config.
func lintCmd() *cobra.Command {
	var text bool
	cmd := &cobra.Command{
		Use:   "lint <fragment.yaml> [more.yaml ...]",
		Short: "validate project config fragment(s) without installing them",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			invalid := 0
			for _, path := range args {
				res := config.LintFragment(path)
				if text {
					renderLintText(res)
				} else {
					fmt.Println(res.JSON())
				}
				if !res.Valid {
					invalid++
				}
			}
			if invalid > 0 {
				return fmt.Errorf("%d fragment(s) invalid", invalid)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&text, "text", false, "human-readable output (default: JSON)")
	return cmd
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
