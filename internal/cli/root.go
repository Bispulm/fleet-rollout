// Package cli implements rolloutctl, the operator command line.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/Bispulm/fleet-rollout/internal/client"
)

const (
	envAPIURL     = "ROLLOUT_API_URL"
	envAdminToken = "ROLLOUT_ADMIN_TOKEN" //nolint:gosec // The name of a variable, not a credential.
	defaultAPIURL = "http://127.0.0.1:18100"
	outputTable   = "table"
	outputJSON    = "json"
)

const rootLong = `rolloutctl manages staged software rollouts across a device fleet.

Environment:
  ` + envAPIURL + `      API address (default ` + defaultAPIURL + `)
  ` + envAdminToken + `  Administrative token (required)

The token is only read from the environment, so it stays out of shell history
and process listings.`

// app holds what every command needs.
type app struct {
	stdout io.Writer
	getenv func(string) string
	output string
}

// client builds an API client from the environment.
func (a *app) client() (*client.Client, error) {
	token := a.getenv(envAdminToken)
	if token == "" {
		return nil, errors.New(envAdminToken + " is not set; export the token the server was started with")
	}
	url := a.getenv(envAPIURL)
	if url == "" {
		url = defaultAPIURL
	}
	return client.New(url, token, nil), nil
}

// Execute runs rolloutctl with the given arguments. It returns any failure rather
// than exiting, and cancels in-flight requests when ctx ends.
func Execute(ctx context.Context, args []string, stdout, stderr io.Writer, getenv func(string) string) error {
	a := &app{stdout: stdout, getenv: getenv}
	root := &cobra.Command{
		Use:           "rolloutctl",
		Short:         "Manage staged software rollouts across a device fleet",
		Long:          rootLong,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	root.PersistentFlags().StringVarP(&a.output, "output", "o", outputTable, "output format: table or json")
	root.PersistentPreRunE = func(*cobra.Command, []string) error {
		if a.output != outputTable && a.output != outputJSON {
			return fmt.Errorf("--output must be %s or %s, got %q", outputTable, outputJSON, a.output)
		}
		return nil
	}
	root.AddCommand(a.keyCommand(), a.fleetCommand(), a.deviceCommand(), a.artifactCommand(), a.rolloutCommand())
	return root.ExecuteContext(ctx)
}
