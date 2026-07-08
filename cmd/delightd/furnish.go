package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"delightd/pkg/kube"
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
	return newFurnishCmd(kubectlRunner, defaultHealthReader())
}

// defaultHealthReader builds the client-go client lazily on first health call: `up` and
// `down` (still kubectl) work even with no kubeconfig present, and the client is built
// once and reused across pieces. Migrating up/down to server-side apply is the next
// step; health -- the read path -- moves first.
func defaultHealthReader() healthReader {
	var (
		c        *kube.Client
		buildErr error
		once     sync.Once
	)
	return func(ctx context.Context, pieceDir string) ([]kubeItem, error) {
		once.Do(func() { c, buildErr = kube.FromKubeconfig("") })
		if buildErr != nil {
			return nil, fmt.Errorf("furnish health: %w", buildErr)
		}
		return kubeHealthReader(c)(ctx, pieceDir)
	}
}

// newFurnishCmd builds the command tree over injectable seams: a runner for the
// kubectl-backed verbs (up/down) and a healthReader for the read path. Tests pass a
// recorder and a fake reader, the same pattern as the events publisher's produce seam.
func newFurnishCmd(run furnishRunner, readHealth healthReader) *cobra.Command {
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
				items, err := readHealth(c.Context(), filepath.Join(kubeDir, p))
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
