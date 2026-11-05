package cli

import (
	"bufio"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/signing"
)

const deviceBatchSize = 1000

// printJSON writes a value as indented JSON.
func (a *app) printJSON(v any) error {
	encoder := json.NewEncoder(a.stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(v)
}

// table returns a writer that aligns tab-separated columns. Call Flush when done.
func (a *app) table() *tabwriter.Writer {
	return tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
}

func parseLabels(flag string, pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	labels := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("--%s: %q is not key=value", flag, pair)
		}
		labels[key] = value
	}
	return labels, nil
}

func (a *app) fleetCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "fleet", Short: "Manage fleets"}

	var labelPairs []string
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a fleet",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			labels, err := parseLabels("label", labelPairs)
			if err != nil {
				return err
			}
			fleet, err := c.CreateFleet(cmd.Context(), apiv1.CreateFleetRequest{Name: args[0], Labels: labels})
			if err != nil {
				return err
			}
			return a.printFleet(fleet)
		},
	}
	create.Flags().StringArrayVar(&labelPairs, "label", nil, "label as key=value; repeatable")

	get := &cobra.Command{
		Use:   "get <name>",
		Short: "Show a fleet",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			fleet, err := c.GetFleet(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.printFleet(fleet)
		},
	}
	cmd.AddCommand(create, get)
	return cmd
}

func (a *app) printFleet(fleet apiv1.Fleet) error {
	if a.output == outputJSON {
		return a.printJSON(fleet)
	}
	_, err := fmt.Fprintf(a.stdout, "Fleet %s (%s), created %s\n", fleet.Name, fleet.ID, fleet.CreatedAt.Format("2006-01-02 15:04:05Z07:00"))
	return err
}

func (a *app) deviceCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "device", Short: "Manage devices"}
	cmd.AddCommand(a.deviceRegisterCommand(), a.deviceSyntheticCommand(), a.deviceListCommand(), a.deviceSetStatusCommand())
	return cmd
}

func (a *app) deviceRegisterCommand() *cobra.Command {
	var fleet, id, platform string
	var labelPairs []string
	var installedRelease int64
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register one device and print its token, which is shown only once",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			labels, err := parseLabels("label", labelPairs)
			if err != nil {
				return err
			}
			devices, err := c.RegisterDevices(cmd.Context(), fleet, []apiv1.DeviceSpec{{
				ID: id, Platform: platform, Labels: labels, InstalledRelease: installedRelease,
			}})
			if err != nil {
				return err
			}
			if a.output == outputJSON {
				return a.printJSON(apiv1.DeviceList{Devices: devices})
			}
			_, err = fmt.Fprintf(a.stdout, "Registered %s\nToken (shown only once): %s\n", devices[0].ID, devices[0].Token)
			return err
		},
	}
	cmd.Flags().StringVar(&fleet, "fleet", "", "fleet name")
	cmd.Flags().StringVar(&id, "id", "", "device ID")
	cmd.Flags().StringVar(&platform, "platform", "", "platform as os/arch")
	cmd.Flags().StringArrayVar(&labelPairs, "label", nil, "label as key=value; repeatable")
	cmd.Flags().Int64Var(&installedRelease, "installed-release", 0, "release counter currently installed")
	mustMarkRequired(cmd, "fleet", "id", "platform")
	return cmd
}

func (a *app) deviceSyntheticCommand() *cobra.Command {
	var fleet, platform, prefix, tokensFile string
	var labelPairs []string
	var count int
	cmd := &cobra.Command{
		Use:   "register-synthetic",
		Short: "Register a batch of simulated devices named <prefix><number>",
		Long: `Register a batch of simulated devices named <prefix><number>.

Registration happens in batches of 1000. Each batch is all or nothing, and if a
later batch fails, earlier ones stay registered. Tokens are discarded unless
--tokens-file is given, since they can't be retrieved afterward.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			labels, err := parseLabels("label", labelPairs)
			if err != nil {
				return err
			}
			if count < 1 {
				return errors.New("--count must be at least 1")
			}

			var tokens *bufio.Writer
			if tokensFile != "" {
				// O_EXCL: overwriting a tokens file would lose credentials for good.
				file, err := os.OpenFile(tokensFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // The operator chose this path.
				if err != nil {
					return fmt.Errorf("open tokens file: %w", err)
				}
				defer func() { _ = file.Close() }()
				tokens = bufio.NewWriter(file)
			}

			registered := 0
			for registered < count {
				size := min(deviceBatchSize, count-registered)
				specs := make([]apiv1.DeviceSpec, size)
				for i := range specs {
					specs[i] = apiv1.DeviceSpec{
						ID:       fmt.Sprintf("%s%05d", prefix, registered+i),
						Platform: platform,
						Labels:   labels,
					}
				}
				devices, err := c.RegisterDevices(cmd.Context(), fleet, specs)
				if err != nil {
					return fmt.Errorf("after registering %d devices: %w", registered, err)
				}
				if tokens != nil {
					if err := writeTokens(tokens, devices); err != nil {
						return err
					}
				}
				registered += size
			}

			note := "Tokens were discarded; pass --tokens-file to keep them."
			if tokens != nil {
				note = "Tokens were written to " + tokensFile + "."
			}
			_, err = fmt.Fprintf(a.stdout, "Registered %d devices in fleet %s. %s\n", registered, fleet, note)
			return err
		},
	}
	cmd.Flags().StringVar(&fleet, "fleet", "", "fleet name")
	cmd.Flags().StringVar(&platform, "platform", "", "platform as os/arch")
	cmd.Flags().StringVar(&prefix, "prefix", "sim-", "device ID prefix")
	cmd.Flags().IntVar(&count, "count", 0, "number of devices")
	cmd.Flags().StringArrayVar(&labelPairs, "label", nil, "label as key=value; repeatable")
	cmd.Flags().StringVar(&tokensFile, "tokens-file", "", "write device tokens here, creating it with mode 600")
	mustMarkRequired(cmd, "fleet", "platform", "count")
	return cmd
}

// writeTokens appends a batch and flushes it, so that a failure in a later batch
// doesn't lose the tokens of devices already registered.
func writeTokens(w *bufio.Writer, devices []apiv1.Device) error {
	for _, d := range devices {
		if _, err := fmt.Fprintf(w, "%s %s\n", d.ID, d.Token); err != nil {
			return fmt.Errorf("write tokens file: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write tokens file: %w", err)
	}
	return nil
}

func (a *app) deviceListCommand() *cobra.Command {
	var fleet, after string
	var limit int
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List a fleet's devices",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			page, err := c.ListDevices(cmd.Context(), fleet, after, limit)
			if err != nil {
				return err
			}
			if a.output == outputJSON {
				return a.printJSON(page)
			}
			table := a.table()
			_, _ = fmt.Fprintln(table, "DEVICE\tPLATFORM\tSTATUS\tRELEASE")
			for _, d := range page.Devices {
				_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%d\n", d.ID, d.Platform, d.Status, d.InstalledRelease)
			}
			if err := table.Flush(); err != nil {
				return err
			}
			if page.NextAfter != "" {
				_, err = fmt.Fprintf(a.stdout, "More devices follow; continue with --after %s\n", page.NextAfter)
			}
			return err
		},
	}
	cmd.Flags().StringVar(&fleet, "fleet", "", "fleet name")
	cmd.Flags().StringVar(&after, "after", "", "continue after this device ID")
	cmd.Flags().IntVar(&limit, "limit", 100, "maximum devices to show, up to 1000")
	mustMarkRequired(cmd, "fleet")
	return cmd
}

func (a *app) deviceSetStatusCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "set-status <device> <active|excluded|decommissioned>",
		Short: "Change a device's status, which affects rollouts that start afterward",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			device, err := c.UpdateDevice(cmd.Context(), args[0], apiv1.UpdateDeviceRequest{Status: rollout.DeviceStatus(args[1])})
			if err != nil {
				return err
			}
			if a.output == outputJSON {
				return a.printJSON(device)
			}
			_, err = fmt.Fprintf(a.stdout, "Device %s is now %s\n", device.ID, device.Status)
			return err
		},
	}
}

func (a *app) artifactCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "artifact", Short: "Manage artifacts"}

	var name, version, platform, file, keyFile string
	var release, chunkSize int64
	push := &cobra.Command{
		Use:   "push",
		Short: "Sign an artifact with your key, then register and upload it",
		Long: `Sign an artifact with your key, then register and upload it.

The manifest describes the file down to each chunk, and it's signed here, so the private key
stays on this machine. The server only accepts it if the key is on its trust list. Devices
check the signature themselves too. It's safe to repeat this if it gets interrupted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			artifact, err := pushArtifact(cmd, c, signing.Meta{
				Name: name, Version: version, Platform: platform, ReleaseCounter: release, ChunkSize: chunkSize,
			}, file, keyFile)
			if err != nil {
				return err
			}
			return a.printArtifact(artifact)
		},
	}
	push.Flags().StringVar(&name, "name", "", "artifact name")
	push.Flags().StringVar(&version, "version", "", "version label")
	push.Flags().StringVar(&platform, "platform", "", "platform as os/arch")
	push.Flags().Int64Var(&release, "release", 0, "monotonic release counter, used for downgrade protection")
	push.Flags().StringVar(&file, "file", "", "path to the artifact file")
	push.Flags().StringVar(&keyFile, "key", "", "path to your private signing key")
	push.Flags().Int64Var(&chunkSize, "chunk-size", signing.DefaultChunkSize, "bytes a device downloads and verifies at a time")
	mustMarkRequired(push, "name", "version", "platform", "release", "file", "key")

	get := &cobra.Command{
		Use:   "get <id>",
		Short: "Show an artifact",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := a.client()
			if err != nil {
				return err
			}
			artifact, err := c.GetArtifact(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return a.printArtifact(artifact)
		},
	}
	cmd.AddCommand(push, get)
	return cmd
}

// pushArtifact builds and signs a manifest from a file, then registers and uploads it. Both
// server calls can be repeated, so a retry picks up wherever an earlier attempt stopped.
func pushArtifact(cmd *cobra.Command, c *client.Client, meta signing.Meta, path, keyFile string) (apiv1.Artifact, error) {
	encodedKey, err := os.ReadFile(keyFile) //nolint:gosec // Reading the file the operator named is the point.
	if err != nil {
		return apiv1.Artifact{}, fmt.Errorf("read signing key: %w", err)
	}
	key, err := signing.ParsePrivateKey(encodedKey)
	if err != nil {
		return apiv1.Artifact{}, fmt.Errorf("read signing key: %w", err)
	}
	file, err := os.Open(path) //nolint:gosec // Reading the file the operator named is the point.
	if err != nil {
		return apiv1.Artifact{}, fmt.Errorf("open artifact: %w", err)
	}
	defer func() { _ = file.Close() }()

	manifest, err := signing.BuildManifest(meta, file)
	if err != nil {
		return apiv1.Artifact{}, err
	}
	envelope, err := signing.Sign(key, manifest)
	if err != nil {
		return apiv1.Artifact{}, err
	}
	artifact, err := c.CreateArtifact(cmd.Context(), envelope)
	if err != nil {
		return apiv1.Artifact{}, err
	}
	if artifact.ContentReady {
		return artifact, nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return apiv1.Artifact{}, fmt.Errorf("rewind artifact: %w", err)
	}
	if err := c.UploadArtifactContent(cmd.Context(), artifact.ID, file, manifest.SizeBytes); err != nil {
		return apiv1.Artifact{}, err
	}
	artifact.ContentReady = true
	return artifact, nil
}

func (a *app) keyCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "key", Short: "Manage signing keys"}
	var privatePath, publicPath string
	generate := &cobra.Command{
		Use:   "generate",
		Short: "Create an Ed25519 signing key",
		Long: `Create an Ed25519 signing key. The private key signs releases, and stays with you. The public
key goes on the trust list of the server and of every device.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			key, err := signing.GenerateKey()
			if err != nil {
				return err
			}
			encodedPrivate, err := signing.MarshalPrivateKey(key)
			if err != nil {
				return err
			}
			public, ok := key.Public().(ed25519.PublicKey)
			if !ok {
				return errors.New("not an Ed25519 key")
			}
			encodedPublic, err := signing.MarshalPublicKey(public)
			if err != nil {
				return err
			}
			// Replacing a key by accident would strand every device that trusts it.
			if err := writeNewFile(privatePath, encodedPrivate, 0o600); err != nil {
				return fmt.Errorf("write private key: %w", err)
			}
			if err := writeNewFile(publicPath, encodedPublic, 0o644); err != nil {
				return fmt.Errorf("write public key: %w", err)
			}
			_, err = fmt.Fprintf(a.stdout, "Wrote private key to %s, and public key to %s. Key ID: %s\n",
				privatePath, publicPath, signing.KeyID(public))
			return err
		},
	}
	generate.Flags().StringVar(&privatePath, "private-key", "", "where to write the private key")
	generate.Flags().StringVar(&publicPath, "public-key", "", "where to write the public key")
	mustMarkRequired(generate, "private-key", "public-key")
	cmd.AddCommand(generate)
	return cmd
}

// writeNewFile fails if the file already exists.
func writeNewFile(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode) //nolint:gosec // The operator chose this path.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Close()
}

func (a *app) printArtifact(artifact apiv1.Artifact) error {
	if a.output == outputJSON {
		return a.printJSON(artifact)
	}
	_, err := fmt.Fprintf(a.stdout, "Artifact %s\n  %s %s for %s, release %d\n  %d bytes, sha256 %s\n  Content ready: %t\n",
		artifact.ID, artifact.Name, artifact.Version, artifact.Platform, artifact.ReleaseCounter, artifact.SizeBytes, artifact.SHA256, artifact.ContentReady)
	return err
}

// mustMarkRequired marks flags as required. It only fails if a flag doesn't exist,
// which is a programming error caught the first time the command is built.
func mustMarkRequired(cmd *cobra.Command, names ...string) {
	for _, name := range names {
		if err := cmd.MarkFlagRequired(name); err != nil {
			panic(err)
		}
	}
}
