// Command rollout-sim runs a rollout across a fleet of simulated devices, each one the real
// agent, with faults injected around it.
//
// "rollout-sim run" is deterministic. It runs the control plane in process on a fake clock,
// in a scratch database that it creates and then drops, so a seed always reproduces the same
// run. "rollout-sim live" drives a rolloutd that's already running, over real HTTP, in real
// time.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Bispulm/fleet-rollout/internal/db"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/signing"
	"github.com/Bispulm/fleet-rollout/internal/sim"
)

const (
	databaseEnv   = "ROLLOUT_SIM_DATABASE_URL"
	adminTokenEnv = "ROLLOUT_ADMIN_TOKEN" //nolint:gosec // The name of a variable, not a credential.
	setupTimeout  = 30 * time.Second
)

func main() {
	if err := runProcess(); err != nil {
		fmt.Fprintln(os.Stderr, "rollout-sim:", err)
		os.Exit(1)
	}
}

func runProcess() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr)
}

const usage = `usage:
  rollout-sim run   [flags]   deterministic, in process, in a scratch database
  rollout-sim live  [flags]   real time, against a running rolloutd
  rollout-sim fleet [flags]   real time, devices for a fleet that already exists

Run "rollout-sim <command> -h" for flags.`

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usage)
		return errors.New("a command is required")
	}
	switch args[0] {
	case "run":
		return runDeterministic(ctx, args[1:], getenv, stdout, stderr)
	case "live":
		return runLive(ctx, args[1:], getenv, stdout, stderr)
	case "fleet":
		return runFleet(ctx, args[1:], stdout, stderr)
	case "-h", "--help", "help":
		_, err := fmt.Fprintln(stdout, usage)
		return err
	default:
		_, _ = fmt.Fprintln(stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// common holds the flags both modes share.
type common struct {
	preset  *string
	seed    *int64
	devices *int
	fleet   *string
	soak    *time.Duration
	maxTime *time.Duration
	asJSON  *bool
	// Rates below zero leave the preset's own.
	dropRate       *float64
	badReleaseRate *float64
	onBreach       *string
}

func commonFlags(flags *flag.FlagSet, soak, maxTime time.Duration) common {
	return common{
		preset:  flags.String("preset", "flaky", "scenario: "+strings.Join(sim.Presets(), ", ")),
		seed:    flags.Int64("seed", 1, "seed for every fault decision"),
		devices: flags.Int("devices", 200, "number of simulated devices"),
		fleet:   flags.String("fleet", "", "fleet name, which must be new"),
		soak:    flags.Duration("soak", soak, "soak time for each stage"),
		maxTime: flags.Duration("max-duration", maxTime, "give up after this long"),
		asJSON:  flags.Bool("json", false, "print the full report as JSON"),
		dropRate: flags.Float64("drop-rate", -1,
			"share of device requests that fail before reaching the server (default: the preset's)"),
		badReleaseRate: flags.Float64("bad-release-rate", -1,
			"share of devices on which the release fails its health check (default: the preset's)"),
		onBreach: flags.String("on-breach", "halt", "what a breach does: halt, or rollback to restore what devices ran before"),
	}
}

func (c common) scenario(defaultFleet string) (sim.Scenario, error) {
	if *c.devices < 1 || *c.devices > 100_000 {
		return sim.Scenario{}, errors.New("--devices must be between 1 and 100000")
	}
	sc, err := sim.Preset(*c.preset, *c.seed, *c.devices, *c.soak)
	if err != nil {
		return sim.Scenario{}, err
	}
	for _, rate := range []struct {
		name  string
		value float64
		dst   *float64
	}{
		{"--drop-rate", *c.dropRate, &sc.Faults.Drop},
		{"--bad-release-rate", *c.badReleaseRate, &sc.Faults.Unhealthy},
	} {
		switch {
		case rate.value < 0:
		case rate.value > 1:
			return sim.Scenario{}, fmt.Errorf("%s must be between 0 and 1", rate.name)
		default:
			*rate.dst = rate.value
		}
	}
	sc.Fleet = *c.fleet
	if sc.Fleet == "" {
		sc.Fleet = defaultFleet
	}
	switch onBreach := rollout.OnBreach(*c.onBreach); onBreach {
	case rollout.OnBreachHalt, rollout.OnBreachRollback:
		sc.Policy.Safety.OnBreach = onBreach
	default:
		return sim.Scenario{}, fmt.Errorf("--on-breach must be halt or rollback, got %q", *c.onBreach)
	}
	sc.MaxDuration = *c.maxTime
	return sc, nil
}

func runDeterministic(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("rollout-sim run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	c := commonFlags(flags, 10*time.Minute, 24*time.Hour)
	serverURL := flags.String("database-url", "", "Postgres server to create the scratch database on; "+databaseEnv+" works too")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *serverURL == "" {
		*serverURL = getenv(databaseEnv)
	}
	if *serverURL == "" {
		return errors.New("--database-url or " + databaseEnv + " is required")
	}
	// The database is new, so the name can't clash, and it's part of what the seed fixes.
	sc, err := c.scenario(fmt.Sprintf("sim-%d", *c.seed))
	if err != nil {
		return err
	}

	scratchURL, drop, err := createScratch(ctx, *serverURL)
	if err != nil {
		return err
	}
	defer drop()
	pool, err := db.Open(ctx, scratchURL, 4)
	if err != nil {
		return fmt.Errorf("open scratch database: %w", err)
	}
	defer pool.Close()
	if _, err := db.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate scratch database: %w", err)
	}
	report, err := sim.Run(ctx, pool, sc)
	if err != nil {
		return err
	}
	return printReport(stdout, report, *c.asJSON)
}

// createScratch creates an empty database and returns its URL, and a function that drops it.
func createScratch(ctx context.Context, serverURL string) (string, func(), error) {
	suffix := make([]byte, 8)
	_, _ = rand.Read(suffix)
	name := "rollout_sim_" + hex.EncodeToString(suffix)
	parsed, err := url.Parse(serverURL)
	if err != nil {
		return "", nil, fmt.Errorf("parse database URL: %w", err)
	}

	setup, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	admin, err := pgx.Connect(setup, serverURL)
	if err != nil {
		return "", nil, fmt.Errorf("connect to database server: %w", err)
	}
	defer func() { _ = admin.Close(context.WithoutCancel(setup)) }()
	if _, err := admin.Exec(setup, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", nil, fmt.Errorf("create scratch database: %w", err)
	}

	drop := func() {
		// This runs even when the run was interrupted, so it gets a context of its own.
		ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
		defer cancel()
		admin, err := pgx.Connect(ctx, serverURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "rollout-sim: couldn't drop scratch database %s: %v\n", name, err)
			return
		}
		defer func() { _ = admin.Close(ctx) }()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			fmt.Fprintf(os.Stderr, "rollout-sim: couldn't drop scratch database %s: %v\n", name, err)
		}
	}
	parsed.Path = "/" + name
	return parsed.String(), drop, nil
}

func runLive(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("rollout-sim live", flag.ContinueOnError)
	flags.SetOutput(stderr)
	c := commonFlags(flags, 15*time.Second, 15*time.Minute)
	apiURL := flags.String("api", "http://127.0.0.1:18100", "rolloutd address")
	keyFile := flags.String("key", "", "private key to sign the simulated release with; rolloutd must trust it")
	trustFile := flags.String("trust-keys", "", "public keys the devices trust (default: the signing key)")
	latency := flags.Duration("latency", 0, "add up to this much latency to each device request")
	quiet := flags.Bool("quiet", false, "don't print progress")
	if err := flags.Parse(args); err != nil {
		return err
	}
	token := getenv(adminTokenEnv)
	if token == "" {
		return errors.New(adminTokenEnv + " is required")
	}
	if *keyFile == "" {
		return errors.New("--key is required")
	}
	// A server keeps every fleet, so each live run gets a new one.
	sc, err := c.scenario(fmt.Sprintf("sim-%d-%d", *c.seed, time.Now().Unix()))
	if err != nil {
		return err
	}
	sc.Faults.Latency = *latency

	keyPEM, err := os.ReadFile(*keyFile)
	if err != nil {
		return fmt.Errorf("read signing key: %w", err)
	}
	signer, err := signing.ParsePrivateKey(keyPEM)
	if err != nil {
		return err
	}
	trust, err := trustFor(*trustFile, signer)
	if err != nil {
		return err
	}
	cfg := sim.LiveConfig{Scenario: sc, APIURL: *apiURL, AdminToken: token, Signer: signer, Trust: trust}
	if !*quiet {
		cfg.Progress = stderr
	}
	report, err := sim.RunLive(ctx, cfg)
	if err != nil {
		return err
	}
	return printReport(stdout, report, *c.asJSON)
}

func runFleet(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("rollout-sim fleet", flag.ContinueOnError)
	flags.SetOutput(stderr)
	apiURL := flags.String("api", "http://127.0.0.1:18100", "rolloutd address")
	tokensFile := flags.String("tokens-file", "", "device IDs and tokens, one pair per line, as rolloutctl writes them")
	trustFile := flags.String("trust-keys", "", "public keys the devices trust to sign releases")
	platform := flags.String("platform", "linux/arm64", "the devices' platform")
	initialRelease := flags.Int64("initial-release", 0, "release counter the devices run before any update")
	preset := flags.String("preset", "flaky", "faults: "+strings.Join(sim.Presets(), ", "))
	soak := flags.Duration("soak", 15*time.Second, "scales the preset's outage and restart times")
	seed := flags.Int64("seed", 1, "seed for every fault decision")
	dropRate := flags.Float64("drop-rate", -1, "share of device requests that fail before reaching the server (default: the preset's)")
	badRate := flags.Float64("bad-release-rate", -1, "share of devices on which releases fail their health check (default: the preset's)")
	duration := flags.Duration("duration", 0, "stop after this long (default: run until interrupted)")
	asJSON := flags.Bool("json", false, "print the report as JSON")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *tokensFile == "" || *trustFile == "" {
		return errors.New("--tokens-file and --trust-keys are required")
	}
	faults, err := sim.PresetFaults(*preset, *soak)
	if err != nil {
		return err
	}
	for _, rate := range []struct {
		name  string
		value float64
		dst   *float64
	}{{"--drop-rate", *dropRate, &faults.Drop}, {"--bad-release-rate", *badRate, &faults.Unhealthy}} {
		switch {
		case rate.value < 0:
		case rate.value > 1:
			return fmt.Errorf("%s must be between 0 and 1", rate.name)
		default:
			*rate.dst = rate.value
		}
	}
	devices, err := readTokens(*tokensFile)
	if err != nil {
		return err
	}
	keys, err := os.ReadFile(*trustFile)
	if err != nil {
		return fmt.Errorf("read trust keys: %w", err)
	}
	trust, err := signing.ParseTrustSet(keys)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stderr, "running %d devices; interrupt to stop\n", len(devices))
	report, err := sim.RunFleet(ctx, sim.FleetConfig{
		APIURL: *apiURL, Devices: devices, Trust: trust, Platform: *platform, InitialRelease: *initialRelease,
		Seed: *seed, Faults: faults, Duration: *duration,
	})
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	releases := make(map[string]int, len(report.Releases))
	for release, count := range report.Releases {
		releases[fmt.Sprintf("release %d", release)] = count
	}
	_, err = fmt.Fprintf(stdout, "%d devices after %s: %s\nfailures: %s\nfaults injected: %s; agent crashes %d\n",
		report.Devices, report.Elapsed.Round(time.Second), counts(releases), counts(report.Failures),
		counts(report.Faults), report.Crashes)
	return err
}

// readTokens reads the device IDs and tokens rolloutctl writes, one pair per line.
func readTokens(path string) ([]sim.Credential, error) {
	data, err := os.ReadFile(path) //nolint:gosec // The operator names the file.
	if err != nil {
		return nil, fmt.Errorf("read tokens: %w", err)
	}
	var out []sim.Credential
	for i, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
		case len(fields) == 2:
			out = append(out, sim.Credential{ID: fields[0], Token: fields[1]})
		default:
			return nil, fmt.Errorf("%s line %d: want a device ID and a token", path, i+1)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s has no devices", path)
	}
	return out, nil
}

func trustFor(file string, signer ed25519.PrivateKey) (signing.TrustSet, error) {
	var data []byte
	if file != "" {
		var err error
		if data, err = os.ReadFile(file); err != nil { //nolint:gosec // The operator names the file.
			return signing.TrustSet{}, fmt.Errorf("read trust keys: %w", err)
		}
	} else {
		public, ok := signer.Public().(ed25519.PublicKey)
		if !ok {
			return signing.TrustSet{}, errors.New("the signing key isn't Ed25519")
		}
		var err error
		if data, err = signing.MarshalPublicKey(public); err != nil {
			return signing.TrustSet{}, err
		}
	}
	return signing.ParseTrustSet(data)
}

func printReport(w io.Writer, r sim.Report, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(r)
	}
	clock := "simulated"
	if r.Mode == "live" {
		clock = "real"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s run of preset %s (drop rate %g, bad release rate %g), seed %d, %d devices\n", r.Mode,
		r.Scenario.Name, r.Scenario.Faults.Drop, r.Scenario.Faults.Unhealthy, r.Scenario.Seed, r.Scenario.Devices)
	fmt.Fprintf(&b, "rollout %s %s after %s (%s time)", r.RolloutID, r.State, r.Elapsed.Round(time.Second), clock)
	if r.HaltReason != "" {
		fmt.Fprintf(&b, ": %s, %s", r.HaltReason, r.HaltDetail)
	}
	if r.TimedOut {
		b.WriteString(", gave up waiting")
	}
	b.WriteString("\n")
	for _, s := range r.Stages {
		fmt.Fprintf(&b, "  stage %d  %-8s %4d devices: %d succeeded, %d failed, %d superseded, %d never assigned\n",
			s.Ordinal, s.State, s.Cohort, s.Succeeded, s.Failed, s.Superseded, s.Pending)
	}
	if rb := r.Rollback; rb != nil {
		fmt.Fprintf(&b, "rolled back: %d of %d updated devices restored, %d unresolved\n", rb.Restored, rb.Needed, rb.Unresolved)
	}
	fmt.Fprintf(&b, "updated %d of %d devices; failures %s; bad installs %d\n",
		r.Updated, r.Scenario.Devices, counts(r.Failures), r.BadInstalls)
	fmt.Fprintf(&b, "faults injected: %s; agent crashes %d\n", counts(r.Faults), r.Crashes)
	fmt.Fprintf(&b, "downloaded %d bytes in all, of a %d-byte artifact\n", r.BytesDownloaded, r.ArtifactBytes)
	fmt.Fprintf(&b, "digest %s\n", r.Digest)
	_, err := io.WriteString(w, b.String())
	return err
}

func counts(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}
