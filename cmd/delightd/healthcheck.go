package main

import (
	"fmt"
	"net/http"

	"github.com/spf13/cobra"

	"delightd/config"
)

// healthcheckCmd is the exec-form probe Docker's HEALTHCHECK instruction runs inside the
// container. The runtime image is `scratch` -- no shell, no curl, nothing but the delightd
// binary itself -- so the self-check has to be delightd probing delightd. It issues the same
// GET /readyz the wrapper's `delightd status` and any external consumer read, on the loopback
// control port (readyz is reachable there regardless of what the container's external port
// mapping looks like), and maps a non-2xx or an unreachable port to a non-zero exit, which is
// exactly what HEALTHCHECK gates the container's health state on. Hidden: this is container
// plumbing invoked by the Docker engine, not a verb an operator types.
func healthcheckCmd() *cobra.Command {
	var addr string
	cmd := &cobra.Command{
		Use:    "healthcheck",
		Short:  "exec-form probe for Docker HEALTHCHECK: GET /readyz on the loopback control port",
		Hidden: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return furnishDo(http.MethodGet, addr, "/readyz")
		},
	}
	cmd.Flags().StringVar(&addr, "control", fmt.Sprintf("127.0.0.1:%d", config.DefaultControlPort),
		"delightd control-port address")
	return cmd
}
