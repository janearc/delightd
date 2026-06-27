package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"delightd/pkg/model"
)

// modelCmd is delightd's model-hosting control surface: it surfaces pkg/model (the
// declared deployments and the LiteLLM config derived from them) as CLI subcommands.
// Built on cobra and JSON-by-default -- the same agent-first, CLI-is-the-contract shape
// as the rest of delightd's commands (cf. lint), so an agent drives it the same way.
// up/down emit idempotent plans; health probes; reconciliation lands in a later step.
func modelCmd() *cobra.Command {
	var deployments string
	cmd := &cobra.Command{
		Use:          "model",
		Short:        "model-hosting deployments (list, render, up, down, health)",
		SilenceUsage: true,
	}
	cmd.PersistentFlags().StringVar(&deployments, "deployments", defaultDeploymentsPath(),
		"path to the deployment set YAML")

	list := &cobra.Command{
		Use:   "list",
		Short: "list the declared model deployments (JSON)",
		RunE: func(_ *cobra.Command, _ []string) error {
			set, err := model.LoadDeploymentSet(deployments)
			if err != nil {
				return err
			}
			return printJSON(set)
		},
	}

	render := &cobra.Command{
		Use:   "render",
		Short: "emit the LiteLLM proxy config derived from the deployments (JSON)",
		RunE: func(_ *cobra.Command, _ []string) error {
			set, err := model.LoadDeploymentSet(deployments)
			if err != nil {
				return err
			}
			return printJSON(model.RenderLiteLLM(set))
		},
	}

	up := &cobra.Command{
		Use:   "up <deployment>",
		Short: "emit the idempotent bring-up plan for a deployment (dry-run)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			set, err := model.LoadDeploymentSet(deployments)
			if err != nil {
				return err
			}
			d, ok := set.ByName(args[0])
			if !ok {
				return fmt.Errorf("unknown deployment %q", args[0])
			}
			return printJSON(map[string]any{
				"command": "up",
				"dry_run": true,
				"note":    "delightd does not launch heavy servers; run idempotent_steps to realise",
				"plan":    d.BringUp(),
			})
		},
	}

	down := &cobra.Command{
		Use:   "down <deployment>",
		Short: "emit the idempotent teardown plan for a deployment (dry-run)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			set, err := model.LoadDeploymentSet(deployments)
			if err != nil {
				return err
			}
			d, ok := set.ByName(args[0])
			if !ok {
				return fmt.Errorf("unknown deployment %q", args[0])
			}
			return printJSON(map[string]any{
				"command": "down",
				"dry_run": true,
				"plan":    d.Teardown(),
			})
		},
	}

	health := &cobra.Command{
		Use:   "health [deployment]",
		Short: "probe deployment endpoint(s); non-zero exit if any is unreachable",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			set, err := model.LoadDeploymentSet(deployments)
			if err != nil {
				return err
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			report, found := set.Health(name)
			if !found {
				return fmt.Errorf("unknown deployment %q", name)
			}
			if err := printJSON(map[string]any{
				"command": "health",
				"healthy": report.Healthy,
				"results": report.Results,
			}); err != nil {
				return err
			}
			if !report.Healthy {
				return fmt.Errorf("one or more deployments unreachable")
			}
			return nil
		},
	}

	cmd.AddCommand(list, render, up, down, health)
	return cmd
}

// defaultDeploymentsPath resolves the deployment set under delightd's config root
// (DELIGHT_CONFIG_ROOT, default ~/etc), overridable with --deployments.
//
// NOTE (blm seam): the descriptor here is delightd-local, but blm owns the wire contract
// (model.v1), which docs/model-hosting.md makes the umbrella. Reconciling the two --
// delightd exposing its deployments as model.v1 descriptors for discovery, and the
// config/paths aligning with good-citizen conventions -- is a later, tracked step.
func defaultDeploymentsPath() string {
	root := os.Getenv("DELIGHT_CONFIG_ROOT")
	if root == "" {
		root = os.ExpandEnv("$HOME/etc")
	}
	return root + "/delightd/model-deployments.yaml"
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}
