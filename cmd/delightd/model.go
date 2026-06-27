package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"delightd/pkg/model"
)

// modelCmd is delightd's model-hosting control surface: the declared model deployments
// and the LiteLLM config derived from them. JSON by default (agent-first). Read-only
// today -- up/down/health and reconciliation land in later steps.
func modelCmd() *cobra.Command {
	var deployments string
	cmd := &cobra.Command{
		Use:   "model",
		Short: "model-hosting deployments (list, render)",
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

	cmd.AddCommand(list, render)
	return cmd
}

// defaultDeploymentsPath resolves the deployment set under delightd's config root
// (DELIGHT_CONFIG_ROOT, default ~/etc), overridable with --deployments.
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
