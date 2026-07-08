package main

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"delightd/pkg/furnish"
)

// furnish talks to the cluster through pkg/furnish (client-go + the kustomize library),
// not the kubectl binary. The sprint-11 ruling that chose kubectl-by-subprocess was
// reversed on receipts 2026-07-08: client-go v0.35 + kustomize/api v0.21.1 hold the
// protobuf v1.36.11 pin. A fake cluster stands in for tests, so no verb needs a live
// cluster to be exercised.

// furnishCmd is delightd's interface to the meubilair set: the no-code kube deployments
// that live one directory per piece under kube/. Same agent-first, CLI-is-the-contract
// shape as model: cobra, JSON by default, idempotent verbs.
func furnishCmd() *cobra.Command {
	return newFurnishCmd(&kubeCluster{})
}

// newFurnishCmd builds the command tree over the cluster seam: up, down, and health all go
// through pkg/furnish. Tests inject a fake cluster, the same pattern as the events
// publisher's produce seam.
func newFurnishCmd(cl cluster) *cobra.Command {
	var kubeDir string
	cmd := &cobra.Command{
		Use:          "furnish",
		Short:        "converge the meubilair pieces declared under kube/ (list, up, down, health)",
		SilenceUsage: true,
	}
	cmd.PersistentFlags().StringVar(&kubeDir, "kube", "kube",
		"per-piece manifest root (a checkout's kube/ directory)")

	// withPiece is the shared load-and-resolve: the name must be declared in the
	// aggregator, and fn gets the piece's directory.
	withPiece := func(name string, fn func(dir string) error) error {
		ps, err := furnish.Pieces(kubeDir)
		if err != nil {
			return err
		}
		for _, p := range ps {
			if p == name {
				return fn(filepath.Join(kubeDir, name))
			}
		}
		return fmt.Errorf("unknown piece %q (declared: %s)", name, strings.Join(ps, ", "))
	}

	list := &cobra.Command{
		Use:   "list",
		Short: "list the declared pieces (JSON)",
		RunE: func(_ *cobra.Command, _ []string) error {
			ps, err := furnish.Pieces(kubeDir)
			if err != nil {
				return err
			}
			return printJSON(map[string]any{"command": "furnish.list", "kube": kubeDir, "pieces": ps})
		},
	}

	up := &cobra.Command{
		Use:   "up <piece>",
		Short: "converge one piece onto its manifests (server-side apply; idempotent)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withPiece(args[0], func(dir string) error {
				if err := cl.apply(c.Context(), dir); err != nil {
					return err
				}
				return printJSON(map[string]any{"command": "furnish.up", "piece": args[0], "applied": true})
			})
		},
	}

	down := &cobra.Command{
		Use:   "down <piece>",
		Short: "remove one piece's objects (client-go delete; absent is success)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withPiece(args[0], func(dir string) error {
				if err := cl.remove(c.Context(), dir); err != nil {
					return err
				}
				return printJSON(map[string]any{"command": "furnish.down", "piece": args[0], "removed": true})
			})
		},
	}

	health := &cobra.Command{
		Use:   "health [piece]",
		Short: "report the health ladder for piece(s); non-zero exit if any is RED",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			ps, err := furnish.Pieces(kubeDir)
			if err != nil {
				return err
			}
			if len(args) == 1 {
				if err := withPiece(args[0], func(string) error { return nil }); err != nil {
					return err
				}
				ps = args[:1]
			}
			healthy := true
			// reports collects the per-piece ladders for the health JSON.
			reports := map[string]any{}
			for _, p := range ps {
				items, err := cl.health(c.Context(), filepath.Join(kubeDir, p))
				if err != nil {
					return err
				}
				ok, ladder := furnish.PieceHealth(items)
				if !ok {
					healthy = false
				}
				reports[p] = ladder
			}
			if err := printJSON(map[string]any{
				"command": "furnish.health", "healthy": healthy, "results": reports,
			}); err != nil {
				return err
			}
			if !healthy {
				return fmt.Errorf("one or more pieces unhealthy")
			}
			return nil
		},
	}

	cmd.AddCommand(list, up, down, health)
	return cmd
}
