// Command rollout-operator reconciles Fleet and Rollout resources in a Kubernetes cluster against
// the control plane's API. The API stays the source of truth: the operator calls it the way
// rolloutctl does, and holds no rollout rules of its own.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Bispulm/fleet-rollout/api/v1alpha1"
	apiclient "github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/operator"
)

const adminTokenEnv = "ROLLOUT_ADMIN_TOKEN" //nolint:gosec // The name of a variable, not a credential.

func main() {
	if err := runProcess(); err != nil {
		fmt.Fprintln(os.Stderr, "rollout-operator:", err)
		os.Exit(1)
	}
}

func runProcess() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := parse(os.Args[1:], os.Getenv, os.Stderr)
	if err != nil {
		return err
	}
	return run(ctx, cfg)
}

type config struct {
	apiURL      string
	adminToken  string
	metricsAddr string
	probeAddr   string
	leaderElect bool
	leaderNS    string
	logLevel    slog.Level
	logOutput   io.Writer
}

func parse(args []string, getenv func(string) string, stderr io.Writer) (config, error) {
	flags := flag.NewFlagSet("rollout-operator", flag.ContinueOnError)
	flags.SetOutput(stderr)
	cfg := config{logOutput: stderr}
	flags.StringVar(&cfg.apiURL, "api", "http://127.0.0.1:18100", "control plane address")
	flags.StringVar(&cfg.metricsAddr, "metrics-bind-address", ":8080", "address for metrics, or 0 to turn them off")
	flags.StringVar(&cfg.probeAddr, "health-probe-bind-address", ":8081", "address for liveness and readiness probes")
	flags.BoolVar(&cfg.leaderElect, "leader-elect", false, "elect a leader, so that more than one replica can run")
	flags.StringVar(&cfg.leaderNS, "leader-election-namespace", "", "namespace for the leader election lease (default: the pod's)")
	logLevel := flags.String("log-level", "info", "debug, info, warn, or error")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if u, err := url.Parse(cfg.apiURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return config{}, fmt.Errorf("--api %q isn't an http or https URL", cfg.apiURL)
	}
	if err := cfg.logLevel.UnmarshalText([]byte(*logLevel)); err != nil {
		return config{}, fmt.Errorf("--log-level: %w", err)
	}
	cfg.adminToken = getenv(adminTokenEnv)
	if cfg.adminToken == "" {
		return config{}, errors.New(adminTokenEnv + " is required")
	}
	return cfg, nil
}

func run(ctx context.Context, cfg config) error {
	handler := slog.NewJSONHandler(cfg.logOutput, &slog.HandlerOptions{Level: cfg.logLevel})
	ctrl.SetLogger(logr.FromSlogHandler(handler))

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	restConfig, err := ctrl.GetConfig()
	if err != nil {
		return fmt.Errorf("find the cluster: %w", err)
	}
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: cfg.metricsAddr},
		HealthProbeBindAddress:  cfg.probeAddr,
		LeaderElection:          cfg.leaderElect,
		LeaderElectionID:        "rollout-operator.rollouts.bispulm.github.io",
		LeaderElectionNamespace: cfg.leaderNS,
	})
	if err != nil {
		return fmt.Errorf("start the manager: %w", err)
	}
	api := apiclient.New(cfg.apiURL, cfg.adminToken, nil)
	if err := (&operator.FleetReconciler{Client: mgr.GetClient(), API: api}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := (&operator.RolloutReconciler{Client: mgr.GetClient(), API: api}).SetupWithManager(mgr); err != nil {
		return err
	}
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	if err := mgr.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	return mgr.Start(ctx)
}
