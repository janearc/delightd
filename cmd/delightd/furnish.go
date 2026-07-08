package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// furnish talks to the cluster through client-go + the kustomize library (the cluster
// seam in furnish_kube.go), not the kubectl binary. The sprint-11 ruling that chose
// kubectl-by-subprocess -- the programmatic stack then dragged k8s.io/* to v0.36 and
// google.golang.org/protobuf off its v1.36.11 pin -- was reversed on receipts 2026-07-08:
// client-go v0.35 + kustomize/api v0.21.1 hold the pin. A fake dynamic client stands in
// for tests, so no verb needs a live cluster to be exercised.

// aggregator is the part of kube/kustomization.yaml furnish reads: the
// resources list. A directory under kube/ is a piece only if it is named
// there -- the declaration, not the filesystem, says what exists.
type aggregator struct {
	Resources []string `yaml:"resources"`
}

// pieces returns the declared pieces under kubeDir, in declaration order.
func pieces(kubeDir string) ([]string, error) {
	path := filepath.Join(kubeDir, "kustomization.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("furnish: no aggregator at %s (run from a checkout, or point --kube at one): %w", path, err)
	}
	var agg aggregator
	if err := yaml.Unmarshal(b, &agg); err != nil {
		return nil, fmt.Errorf("furnish: %s does not parse: %w", path, err)
	}
	if len(agg.Resources) == 0 {
		return nil, fmt.Errorf("furnish: %s declares no resources", path)
	}
	return agg.Resources, nil
}

// kubeItem is the minimal slice of `kubectl get -o json` that health reads:
// enough to judge a Deployment's or StatefulSet's readiness and to name
// everything else (both expose spec.replicas + status.readyReplicas).
type kubeItem struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int32 `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ReadyReplicas int32 `json:"readyReplicas"`
	} `json:"status"`
	// absent marks an object the piece declares but that is not live. It is set by the
	// client-go reader on a NotFound (never from kubectl JSON); pieceHealth reports it
	// RED rather than GREEN-by-existence, so a declared-but-undeployed piece is honest.
	absent bool
}

// pieceHealth walks one piece's rendered objects and reports a ladder:
// a Deployment or StatefulSet is GREEN when readyReplicas meets spec.replicas
// (unset means 1, kube's own default), RED otherwise; any other kind that
// exists is GREEN by existence. The piece is healthy only if nothing is RED.
// StatefulSet matters here because the relocated bus/store pieces (kafka,
// zookeeper, elasticsearch) are StatefulSets: without this a CrashLooping
// kafka-0 would report GREEN by mere existence and furnish health would lie.
func pieceHealth(items []kubeItem) (bool, []map[string]any) {
	healthy := true
	// results is the per-object ladder rendered into the health JSON.
	var results []map[string]any
	for _, it := range items {
		state := "GREEN"
		detail := "present"
		switch {
		case it.absent:
			// Declared but not live: RED, never GREEN-by-existence.
			state = "RED"
			detail = "declared but absent"
			healthy = false
		case it.Kind == "Deployment" || it.Kind == "StatefulSet":
			want := int32(1)
			if it.Spec.Replicas != nil {
				want = *it.Spec.Replicas
			}
			if it.Status.ReadyReplicas < want {
				state = "RED"
				healthy = false
			}
			detail = fmt.Sprintf("%d/%d ready", it.Status.ReadyReplicas, want)
		}
		results = append(results, map[string]any{
			"kind": it.Kind, "name": it.Metadata.Name, "state": state, "detail": detail,
		})
	}
	return healthy, results
}

// furnishCmd is delightd's interface to the meubilair set: the no-code kube
// deployments that live one directory per piece under kube/ (delightd itself
// today; kafka, searxng, chromadb, redis, surrealdb as they move in). Same
// agent-first, CLI-is-the-contract shape as model: cobra, JSON by default,
// idempotent verbs -- an agent drives it the same way.
func furnishCmd() *cobra.Command {
	return newFurnishCmd(&kubeCluster{})
}

// newFurnishCmd builds the command tree over the cluster seam: up, down, and health all
// go through client-go + kustomize. Tests inject a fake cluster (or one backed by a fake
// dynamic client), the same pattern as the events publisher's produce seam.
func newFurnishCmd(cl cluster) *cobra.Command {
	var kubeDir string
	cmd := &cobra.Command{
		Use:          "furnish",
		Short:        "converge the meubilair pieces declared under kube/ (list, up, down, health)",
		SilenceUsage: true,
	}
	cmd.PersistentFlags().StringVar(&kubeDir, "kube", "kube",
		"per-piece manifest root (a checkout's kube/ directory)")

	// withPiece is the shared load-and-resolve: the name must be declared in
	// the aggregator, and fn gets the piece's directory. New per-piece verbs
	// reuse it instead of repeating the lookup + unknown-name error.
	withPiece := func(name string, fn func(dir string) error) error {
		ps, err := pieces(kubeDir)
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
			ps, err := pieces(kubeDir)
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
			ps, err := pieces(kubeDir)
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
				ok, ladder := pieceHealth(items)
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
