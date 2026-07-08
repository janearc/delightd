package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"delightd/config"
)

// furnishCmd drives delightd's operator surface -- converging the meubilair pieces -- over
// the control port. delightd holds the single cluster connection (client-go + the baked
// manifests); the CLI is a thin client, so there is one path to the cluster: the daemon.
// It talks to 127.0.0.1:8088 by default (the localhost-bound control port). Same
// agent-first, CLI-is-the-contract shape as model: cobra, JSON relayed verbatim, the
// daemon's status mapped to the exit code so an agent or the wrapper can gate on it.
func furnishCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:          "furnish",
		Short:        "converge the meubilair pieces via the daemon (list, up, down, health)",
		SilenceUsage: true,
	}
	cmd.PersistentFlags().StringVar(&addr, "control", fmt.Sprintf("127.0.0.1:%d", config.DefaultControlPort),
		"delightd control-port address")

	list := &cobra.Command{
		Use:   "list",
		Short: "list the declared pieces (JSON)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return furnishDo(http.MethodGet, addr, "/furnish/pieces")
		},
	}
	health := &cobra.Command{
		Use:   "health [piece]",
		Short: "report the health ladder; non-zero exit if any piece is RED or INDETERMINATE",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			path := "/furnish/health"
			if len(args) == 1 {
				path += "/" + args[0]
			}
			return furnishDo(http.MethodGet, addr, path)
		},
	}
	up := &cobra.Command{
		Use:   "up <piece>",
		Short: "converge one piece onto its manifests (server-side apply; idempotent)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return furnishDo(http.MethodPost, addr, "/furnish/"+args[0]+"/up")
		},
	}
	down := &cobra.Command{
		Use:   "down <piece>",
		Short: "remove one piece's objects (absent is success)",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return furnishDo(http.MethodPost, addr, "/furnish/"+args[0]+"/down")
		},
	}

	cmd.AddCommand(list, up, down, health)
	return cmd
}

// furnishDo issues one request to the control port, relays the daemon's JSON body to
// stdout verbatim (it is the contract), and maps the status to the exit: a health 503
// (RED or INDETERMINATE) or any other non-2xx becomes a non-zero exit. A connection
// failure names the likely cause -- the daemon is not up.
func furnishDo(method, addr, path string) error {
	req, err := http.NewRequest(method, "http://"+addr+path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("furnish: cannot reach delightd at %s (is the daemon up? try 'delightd status'): %w", addr, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if b := strings.TrimSpace(string(body)); b != "" {
		fmt.Fprintln(os.Stdout, b)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("furnish: %s %s -> %s", method, path, resp.Status)
	}
	return nil
}
