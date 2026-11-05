// Command rollout-agent runs on a device. It polls the control plane for updates, downloads
// and verifies them, installs into an inactive slot, and reports what happened. Installation
// is simulated: an artifact is opaque bytes copied into a slot, and nothing is run.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/agent"
	"github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/signing"
)

const tokenEnv = "ROLLOUT_AGENT_TOKEN" //nolint:gosec // The name of a variable, not a credential.

func main() {
	if err := runProcess(); err != nil {
		fmt.Fprintln(os.Stderr, "rollout-agent:", err)
		os.Exit(1)
	}
}

func runProcess() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Args[1:], os.Getenv, os.Stderr)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) error {
	flags := flag.NewFlagSet("rollout-agent", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiURL := flags.String("api", "", "control plane address")
	deviceID := flags.String("device-id", "", "this device's ID")
	tokenFile := flags.String("token-file", "", "file holding the device token; "+tokenEnv+" works too")
	stateDir := flags.String("state-dir", "", "directory for durable state, downloads, and slots")
	platform := flags.String("platform", "", "this device's platform, as os/arch")
	trustFile := flags.String("trust-keys", "", "public keys trusted to sign releases")
	initialRelease := flags.Int64("initial-release", 0, "release counter running before the first update")
	maxBytes := flags.Int64("max-artifact-bytes", 1<<30, "largest artifact this device accepts")
	logLevel := flags.String("log-level", "info", "debug, info, warn, or error")
	if err := flags.Parse(args); err != nil {
		return err
	}

	switch {
	case *apiURL == "":
		return errors.New("--api is required")
	case !rollout.ValidDeviceID(*deviceID):
		return errors.New("--device-id is required, as letters, digits, '.', '_', and '-'")
	case *stateDir == "":
		return errors.New("--state-dir is required")
	case !rollout.ValidPlatform(*platform):
		return errors.New("--platform must have the form os/arch")
	}
	token, err := readToken(*tokenFile, getenv)
	if err != nil {
		return err
	}
	trustKeys, err := os.ReadFile(*trustFile)
	if err != nil {
		return fmt.Errorf("read trust keys: %w", err)
	}
	trust, err := signing.ParseTrustSet(trustKeys)
	if err != nil {
		return fmt.Errorf("read trust keys: %w", err)
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("--log-level: %w", err)
	}
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: level}))

	checkpoints, err := agent.NewFileCheckpoints(*stateDir)
	if err != nil {
		return err
	}
	partials, err := agent.NewFilePartials(filepath.Join(*stateDir, "partials"))
	if err != nil {
		return err
	}
	installer, err := agent.NewFileInstaller(filepath.Join(*stateDir, "slots"))
	if err != nil {
		return err
	}
	device, err := agent.New(agent.Config{
		DeviceID:         *deviceID,
		Platform:         *platform,
		Trust:            trust,
		MaxArtifactBytes: *maxBytes,
		InitialRelease:   *initialRelease,
	}, agent.Deps{
		Transport:   client.NewDevice(*apiURL, token, nil),
		Checkpoints: checkpoints,
		Partials:    partials,
		Installer:   installer,
		Clock:       realClock{},
		Rand:        rand.Float64,
		Logger:      logger,
	})
	if err != nil {
		return err
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "agent started", slog.String("device_id", *deviceID))
	return device.Run(ctx)
}

// readToken prefers the environment, so a token never has to be on the command line.
func readToken(path string, getenv func(string) string) (string, error) {
	if token := getenv(tokenEnv); token != "" {
		return token, nil
	}
	if path == "" {
		return "", errors.New("a device token is required, in " + tokenEnv + " or --token-file")
	}
	data, err := os.ReadFile(path) //nolint:gosec // The operator chose this path.
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", errors.New("the token file is empty")
	}
	return token, nil
}
