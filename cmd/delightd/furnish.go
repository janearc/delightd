package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// kubectl-by-subprocess is a ruled decision, not a shortcut (sprint 11
// Phase A, issue 77): the programmatic stack (client-go + the kustomize
// library) drags k8s.io/* to v0.36 and google.golang.org/protobuf off its
// v1.36.11 pin -- the dep the generated contract code shares. kubectl is a
// runtime requirement instead, checked fail-loud before any verb touches the
// cluster.

// furnishRunner is the seam between the furnish verbs and kubectl: production
// runs the real binary, tests substitute a recorder. Arguments arrive without
// the program name ("apply", "-k", <dir>).
type furnishRunner func(ctx context.Context, args ...string) ([]byte, error)

// kubectlRunner is the production runner. KUBECONFIG is deliberately left
// alone -- furnish converges whatever cluster the operator's environment
// points at (locally, the k3d cluster).
func kubectlRunner(ctx context.Context, args ...string) ([]byte, error) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		return nil, fmt.Errorf("kubectl not found on PATH; furnish requires it at runtime: %w", err)
	}
	out, err := exec.CommandContext(ctx, "kubectl", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("kubectl %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return out, nil
}

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
		if it.Kind == "Deployment" || it.Kind == "StatefulSet" {
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
	return newFurnishCmd(kubectlRunner)
}

// newFurnishCmd builds the command tree over an injectable runner; tests pass
// a recorder here, the same pattern as the events publisher's produce seam.
func newFurnishCmd(run furnishRunner) *cobra.Command {
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
		Short: "converge one piece onto its manifests (kubectl apply -k; idempotent)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withPiece(args[0], func(dir string) error {
				out, err := run(c.Context(), "apply", "-k", dir)
				if err != nil {
					return err
				}
				return printJSON(map[string]any{
					"command": "furnish.up", "piece": args[0],
					"applied": strings.Split(strings.TrimSpace(string(out)), "\n"),
				})
			})
		},
	}

	down := &cobra.Command{
		Use:   "down <piece>",
		Short: "remove one piece's objects (kubectl delete -k; absent is success)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return withPiece(args[0], func(dir string) error {
				out, err := run(c.Context(), "delete", "-k", dir, "--ignore-not-found=true")
				if err != nil {
					return err
				}
				return printJSON(map[string]any{
					"command": "furnish.down", "piece": args[0],
					"removed": strings.Split(strings.TrimSpace(string(out)), "\n"),
				})
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
				out, err := run(c.Context(), "get", "-k", filepath.Join(kubeDir, p), "-o", "json")
				if err != nil {
					// A piece that will not answer -- undeployed, or any kubectl
					// error -- is RED for that piece, not a reason to abort the
					// whole report. Once the aggregator carries independently-
					// deployed furniture (kafka/elk), one un-applied piece must
					// not blank out a healthy delightd/surrealdb.
					healthy = false
					reports[p] = []map[string]any{{"name": p, "state": "RED", "detail": "unreachable: " + err.Error()}}
					continue
				}
				var got struct {
					Items []kubeItem `json:"items"`
				}
				if err := json.Unmarshal(out, &got); err != nil {
					healthy = false
					reports[p] = []map[string]any{{"name": p, "state": "RED", "detail": "unparseable kubectl output: " + err.Error()}}
					continue
				}
				ok, ladder := pieceHealth(got.Items)
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
