package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/sim"
)

func TestArgumentsAreChecked(t *testing.T) {
	getenv := func(string) string { return "" }
	for name, args := range map[string][]string{
		"no command":       nil,
		"unknown command":  {"walk"},
		"no database":      {"run"},
		"unknown preset":   {"run", "--preset", "nope", "--database-url", "postgres://x"},
		"too many devices": {"run", "--devices", "100001", "--database-url", "postgres://x"},
		"drop rate over 1": {"run", "--drop-rate", "1.5", "--database-url", "postgres://x"},
		"bad rate over 1":  {"run", "--bad-release-rate", "2", "--database-url", "postgres://x"},
		"bad breach":       {"run", "--on-breach", "panic", "--database-url", "postgres://x"},
		"negative minimum": {"run", "--min-observations", "-1", "--database-url", "postgres://x"},
		"no admin token":   {"live", "--key", "release.key"},
		"fleet, no tokens": {"fleet", "--trust-keys", "release.pub"},
		"fleet, no trust":  {"fleet", "--tokens-file", "tokens.txt"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run(t.Context(), args, getenv, &stdout, &stderr); err == nil {
				t.Fatalf("accepted %q", args)
			}
		})
	}
}

func TestABadReleaseMustBeAReleaseCounter(t *testing.T) {
	err := run(t.Context(), []string{"fleet", "--tokens-file", "tokens.txt", "--trust-keys", "release.pub", "--bad-release", "-1"},
		func(string) string { return "" }, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "--bad-release") {
		t.Fatalf("err = %v, want one about --bad-release", err)
	}
}

func TestADeterministicRunPrintsItsReport(t *testing.T) {
	serverURL := os.Getenv("ROLLOUT_TEST_DATABASE_URL")
	if serverURL == "" {
		if os.Getenv("ROLLOUT_TEST_REQUIRE_DB") == "1" {
			t.Fatal("ROLLOUT_TEST_REQUIRE_DB=1 but ROLLOUT_TEST_DATABASE_URL is unset")
		}
		t.Skip("ROLLOUT_TEST_DATABASE_URL is unset; skipping database test")
	}
	getenv := func(name string) string {
		if name == databaseEnv {
			return serverURL
		}
		return ""
	}

	var stdout, stderr bytes.Buffer
	args := []string{"run", "--preset", "clean", "--devices", "20", "--seed", "4", "--json"}
	if err := run(t.Context(), args, getenv, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\n%s", err, stderr.String())
	}
	var report sim.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("the report isn't JSON: %v\n%s", err, stdout.String())
	}
	if report.State != rollout.StateCompleted || report.Updated != 20 || report.Digest == "" {
		t.Fatalf("report = %+v", report)
	}

	stdout.Reset()
	if err := run(t.Context(), args[:len(args)-1], getenv, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\n%s", err, stderr.String())
	}
	if text := stdout.String(); !strings.Contains(text, "updated 20 of 20 devices") || !strings.Contains(text, report.Digest) {
		t.Fatalf("summary doesn't match the report:\n%s", text)
	}
}

func TestRatesOverrideThePreset(t *testing.T) {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	c := commonFlags(flags, time.Minute, time.Hour)
	if err := flags.Parse([]string{"--preset", "clean", "--drop-rate", "0.2", "--bad-release-rate", "0.05"}); err != nil {
		t.Fatal(err)
	}
	sc, err := c.scenario("fleet")
	if err != nil {
		t.Fatal(err)
	}
	if sc.Faults.Drop != 0.2 || sc.Faults.Unhealthy != 0.05 {
		t.Fatalf("faults = %+v", sc.Faults)
	}

	flags = flag.NewFlagSet("test", flag.ContinueOnError)
	c = commonFlags(flags, time.Minute, time.Hour)
	if err := flags.Parse([]string{"--min-observations", "20", "--on-breach", "rollback"}); err != nil {
		t.Fatal(err)
	}
	if sc, err = c.scenario("fleet"); err != nil {
		t.Fatal(err)
	}
	for i, stage := range sc.Policy.Stages {
		if stage.MinObservations == nil || *stage.MinObservations != 20 {
			t.Fatalf("stage %d minimum = %v, want 20", i, stage.MinObservations)
		}
	}
	if sc.Policy.Safety.OnBreach != "rollback" {
		t.Fatalf("on breach = %q", sc.Policy.Safety.OnBreach)
	}

	// Left unset, the preset's own rates stand.
	flags = flag.NewFlagSet("test", flag.ContinueOnError)
	c = commonFlags(flags, time.Minute, time.Hour)
	if err := flags.Parse([]string{"--preset", "bad-release"}); err != nil {
		t.Fatal(err)
	}
	if sc, err = c.scenario("fleet"); err != nil || sc.Faults.Unhealthy != 0.3 || sc.Faults.Drop != 0.05 {
		t.Fatalf("faults = %+v, %v", sc.Faults, err)
	}
}

func TestTokensAreReadAsRolloutctlWritesThem(t *testing.T) {
	dir := t.TempDir()
	good := dir + "/tokens.txt"
	if err := os.WriteFile(good, []byte("sim-00001 abc\nsim-00002 def\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readTokens(good)
	if err != nil || len(got) != 2 || got[1].ID != "sim-00002" || got[1].Token != "def" {
		t.Fatalf("readTokens = %+v, %v", got, err)
	}
	bad := dir + "/bad.txt"
	if err := os.WriteFile(bad, []byte("sim-00001\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readTokens(bad); err == nil {
		t.Fatal("a line without a token was accepted")
	}
}
