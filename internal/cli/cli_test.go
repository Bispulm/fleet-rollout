package cli_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/api"
	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/artifactstore"
	"github.com/Bispulm/fleet-rollout/internal/cli"
	"github.com/Bispulm/fleet-rollout/internal/client"
	"github.com/Bispulm/fleet-rollout/internal/rollout"
	"github.com/Bispulm/fleet-rollout/internal/signing"
	"github.com/Bispulm/fleet-rollout/internal/signing/signingtest"
	"github.com/Bispulm/fleet-rollout/internal/store"
	"github.com/Bispulm/fleet-rollout/internal/testdb"
)

const adminToken = "cli-test-credential-value"

// env runs rolloutctl in-process against a real server and database.
type env struct {
	t      *testing.T
	getenv func(string) string
	base   string
	store  *store.Store
	key    string // Path to a private key the server trusts.
}

func newEnv(t *testing.T) env {
	t.Helper()
	t.Parallel()
	st := store.New(testdb.New(t))
	signer := signingtest.NewKey(t)
	encodedKey, err := signing.MarshalPrivateKey(signer)
	if err != nil {
		t.Fatalf("MarshalPrivateKey: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "release.key")
	if err := os.WriteFile(keyPath, encodedKey, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	artifacts, err := artifactstore.NewFilesystem(t.TempDir())
	if err != nil {
		t.Fatalf("NewFilesystem: %v", err)
	}
	srv := api.New(api.Options{
		Logger:     slog.New(slog.DiscardHandler),
		Ready:      func(context.Context) error { return nil },
		Store:      st,
		AdminToken: adminToken,
		Trust:      signingtest.Trust(t, signer),
		Artifacts:  artifacts,
	})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	vars := map[string]string{"ROLLOUT_API_URL": ts.URL, "ROLLOUT_ADMIN_TOKEN": adminToken}
	return env{t: t, getenv: func(k string) string { return vars[k] }, base: ts.URL, store: st, key: keyPath}
}

func (e env) run(args ...string) (string, error) {
	e.t.Helper()
	var stdout, stderr bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := cli.Execute(ctx, args, &stdout, &stderr, e.getenv)
	return stdout.String(), err
}

func (e env) mustRun(args ...string) string {
	e.t.Helper()
	out, err := e.run(args...)
	if err != nil {
		e.t.Fatalf("rolloutctl %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// mustJSON runs a command with JSON output and decodes the result.
func mustJSON[T any](e env, args ...string) T {
	e.t.Helper()
	var out T
	raw := e.mustRun(append(args, "-o", "json")...)
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		e.t.Fatalf("decode %q: %v", raw, err)
	}
	return out
}

// seed creates a fleet of n devices and an artifact, returning the artifact ID.
func (e env) seed(n int) string {
	e.t.Helper()
	e.mustRun("fleet", "create", "lab-a", "--label", "site=lab")
	e.mustRun("device", "register-synthetic", "--fleet", "lab-a", "--count", fmt.Sprint(n),
		"--platform", "linux/arm64", "--label", "site=lab")
	file := filepath.Join(e.t.TempDir(), "demo-app.bin")
	if err := os.WriteFile(file, []byte("synthetic artifact bytes"), 0o600); err != nil {
		e.t.Fatalf("write artifact: %v", err)
	}
	artifact := mustJSON[apiv1.Artifact](e, "artifact", "push", "--name", "demo-app", "--version", "1.4.0",
		"--platform", "linux/arm64", "--release", "14", "--file", file, "--key", e.key, "--chunk-size", "1024")
	return artifact.ID
}

func (e env) createRollout(artifactID string, extra ...string) apiv1.Rollout {
	e.t.Helper()
	args := []string{
		"rollout", "create", "--fleet", "lab-a", "--name", "demo", "--artifact", artifactID,
		"--stages", "1,10,50,100", "--soak", "10m", "--max-failure-rate", "2%",
	}
	return mustJSON[apiv1.Rollout](e, append(args, extra...)...)
}

func TestRegisterCreateStartAndInspectACohort(t *testing.T) {
	e := newEnv(t)
	artifact := e.seed(100)

	created := e.createRollout(artifact)
	if created.State != rollout.StateDraft || created.Policy.Safety.MaxFailureBPS != 200 {
		t.Fatalf("created = %+v", created)
	}
	if got := created.Policy.Stages[0].Soak.Std(); got != 10*time.Minute {
		t.Fatalf("soak = %v", got)
	}

	if _, err := e.run("rollout", "cohort", created.ID); err == nil || !strings.Contains(err.Error(), "start") {
		t.Fatalf("cohort of a draft: error = %v, want one explaining it isn't started", err)
	}

	status := e.mustRun("rollout", "status", created.ID)
	for _, want := range []string{"demo", "lab-a", "draft", "2.00%", "90.00%"} {
		if !strings.Contains(status, want) {
			t.Errorf("draft status is missing %q:\n%s", want, status)
		}
	}

	started := mustJSON[apiv1.Rollout](e, "rollout", "start", created.ID)
	if started.State != rollout.StateRunning || *started.MemberCount != 100 {
		t.Fatalf("started = %+v", started)
	}

	status = e.mustRun("rollout", "status", created.ID)
	for _, want := range []string{"running", "100", "active", "pending"} {
		if !strings.Contains(status, want) {
			t.Errorf("running status is missing %q:\n%s", want, status)
		}
	}

	// The CLI's order must match a calculation from the rollout ID and device IDs.
	ids := make([]string, 100)
	for i := range ids {
		ids[i] = fmt.Sprintf("sim-%05d", i)
	}
	want := rollout.RankDevices(created.ID, ids)

	first := mustJSON[apiv1.CohortPage](e, "rollout", "cohort", created.ID, "--stage", "1")
	if len(first.Entries) != 1 || first.Entries[0].DeviceID != want[0].DeviceID {
		t.Fatalf("stage 1 = %+v, want device %s", first.Entries, want[0].DeviceID)
	}
	all := mustJSON[apiv1.CohortPage](e, "rollout", "cohort", created.ID, "--limit", "1000")
	for i, entry := range all.Entries {
		if entry.DeviceID != want[i].DeviceID {
			t.Fatalf("position %d = %s, want %s", i+1, entry.DeviceID, want[i].DeviceID)
		}
	}

	table := e.mustRun("rollout", "cohort", created.ID, "--stage", "2")
	if !strings.Contains(table, want[1].DeviceID) || strings.Contains(table, want[0].DeviceID) {
		t.Fatalf("stage 2 table should show the second cohort only:\n%s", table)
	}
}

func TestLifecycleCommandsAndHistory(t *testing.T) {
	e := newEnv(t)
	r := e.createRollout(e.seed(100))

	if _, err := e.run("rollout", "pause", r.ID); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("pausing a draft: error = %v", err)
	}
	if _, err := e.run("rollout", "start", r.ID, "--if-revision", "99"); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("stale revision: error = %v", err)
	}

	e.mustRun("rollout", "start", r.ID, "--if-revision", "1")
	paused := mustJSON[apiv1.Rollout](e, "rollout", "pause", r.ID, "--reason", "looks odd")
	if paused.State != rollout.StatePaused {
		t.Fatalf("paused = %+v", paused)
	}
	resumed := mustJSON[apiv1.Rollout](e, "rollout", "resume", r.ID)
	if resumed.State != rollout.StateRunning {
		t.Fatalf("resumed = %+v", resumed)
	}
	aborted := mustJSON[apiv1.Rollout](e, "rollout", "abort", r.ID, "--reason", "done testing")
	if aborted.State != rollout.StateAborted {
		t.Fatalf("aborted = %+v", aborted)
	}

	history := e.mustRun("rollout", "history", r.ID)
	for _, want := range []string{"start", "pause", "resume", "abort", "looks odd", "done testing", "admin"} {
		if !strings.Contains(history, want) {
			t.Errorf("history is missing %q:\n%s", want, history)
		}
	}
}

func TestCreateShowsEveryPolicyViolation(t *testing.T) {
	e := newEnv(t)
	artifact := e.seed(10)
	_, err := e.run("rollout", "create", "--fleet", "lab-a", "--name", "demo", "--artifact", artifact,
		"--stages", "10,100", "--max-failure-rate", "2%", "--min-observations", "5,1")
	if err == nil || !strings.Contains(err.Error(), "stages[0].min_observations") {
		t.Fatalf("error = %v, want the violation for the impossible first stage", err)
	}
}

func TestCreatePassesEveryPolicyFlag(t *testing.T) {
	e := newEnv(t)
	artifact := e.seed(100)
	r := e.createRollout(artifact,
		"--deadline", "2h", "--min-observations", "1,2,3,4",
		"--rollout-max-failure-rate", "1.5%", "--required-coverage", "95%", "--max-unknown", "0.25%",
		"--stale-after", "20m", "--on-breach", "halt", "--selector", "site=lab",
		"--exclude-device", "sim-00003", "--id", "11111111-1111-4111-8111-111111111111")

	s := r.Policy.Safety
	if s.RolloutMaxFailureBPS != 150 || s.RequiredCoverageBPS != 9500 || s.MaxUnknownBPS != 25 ||
		s.StaleAfter.Std() != 20*time.Minute || s.OnBreach != rollout.OnBreachHalt {
		t.Fatalf("safety = %+v", s)
	}
	if r.ID != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("id = %s", r.ID)
	}
	if r.Policy.Selector["site"] != "lab" || !slices.Contains(r.Policy.ExcludeDevices, "sim-00003") {
		t.Fatalf("policy = %+v", r.Policy)
	}
	for i, stage := range r.Policy.Stages {
		if stage.MinObservations != i+1 || stage.Deadline.Std() != 2*time.Hour {
			t.Errorf("stage %d = %+v", i, stage)
		}
	}
}

func TestRejectsBadFlagsLocally(t *testing.T) {
	e := newEnv(t)
	base := []string{"rollout", "create", "--fleet", "lab-a", "--name", "demo", "--artifact", "x"}
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"failure rate required", []string{"--stages", "100"}, "max-failure-rate"},
		{"stages required", []string{"--max-failure-rate", "2%"}, "stages"},
		{"rate without percent sign", []string{"--stages", "100", "--max-failure-rate", "2"}, "max-failure-rate"},
		{"rate above 100%", []string{"--stages", "100", "--max-failure-rate", "101%"}, "max-failure-rate"},
		{"negative rate", []string{"--stages", "100", "--max-failure-rate", "-1%"}, "max-failure-rate"},
		{"rate too precise", []string{"--stages", "100", "--max-failure-rate", "2.555%"}, "max-failure-rate"},
		{"non-numeric stage", []string{"--stages", "1,x", "--max-failure-rate", "2%"}, "stages"},
		{"mismatched minimums", []string{"--stages", "50,100", "--max-failure-rate", "2%", "--min-observations", "1,2,3"}, "min-observations"},
		{"malformed selector", []string{"--stages", "100", "--max-failure-rate", "2%", "--selector", "noequals"}, "selector"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.run(append(slices.Clone(base), tc.args...)...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func TestRequiresAdminTokenAndExplainsIt(t *testing.T) {
	e := newEnv(t)
	noToken := func(k string) string {
		if k == "ROLLOUT_ADMIN_TOKEN" {
			return ""
		}
		return e.getenv(k)
	}
	var stdout, stderr bytes.Buffer
	err := cli.Execute(t.Context(), []string{"fleet", "get", "lab-a"}, &stdout, &stderr, noToken)
	if err == nil || !strings.Contains(err.Error(), "ROLLOUT_ADMIN_TOKEN") {
		t.Fatalf("error = %v, want one naming the variable", err)
	}
}

func TestDeviceCommands(t *testing.T) {
	e := newEnv(t)
	e.mustRun("fleet", "create", "lab-a")

	one := mustJSON[apiv1.DeviceList](e, "device", "register", "--fleet", "lab-a", "--id", "bench-1",
		"--platform", "linux/arm64", "--label", "site=lab", "--installed-release", "3")
	if len(one.Devices) != 1 || one.Devices[0].Token == "" || one.Devices[0].InstalledRelease != 3 {
		t.Fatalf("registered = %+v", one)
	}

	updated := mustJSON[apiv1.Device](e, "device", "set-status", "bench-1", "excluded")
	if updated.Status != rollout.DeviceExcluded {
		t.Fatalf("updated = %+v", updated)
	}

	list := e.mustRun("device", "list", "--fleet", "lab-a")
	if !strings.Contains(list, "bench-1") || !strings.Contains(list, "excluded") {
		t.Fatalf("list:\n%s", list)
	}
	if strings.Contains(list, one.Devices[0].Token) {
		t.Fatal("device list printed a token")
	}
}

func TestRegisterSyntheticWritesTokensPrivatelyAcrossBatches(t *testing.T) {
	e := newEnv(t)
	e.mustRun("fleet", "create", "lab-a")
	tokens := filepath.Join(t.TempDir(), "tokens.txt")

	out := e.mustRun("device", "register-synthetic", "--fleet", "lab-a", "--count", "1205",
		"--platform", "linux/arm64", "--tokens-file", tokens)
	if !strings.Contains(out, "1205") {
		t.Fatalf("output = %q", out)
	}

	info, err := os.Stat(tokens)
	if err != nil {
		t.Fatalf("stat tokens file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("tokens file mode = %o, want 600", perm)
	}
	raw, err := os.ReadFile(tokens)
	if err != nil {
		t.Fatalf("read tokens file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1205 || !strings.HasPrefix(lines[1204], "sim-01204 frd_") {
		t.Fatalf("tokens file has %d lines, ending %q", len(lines), lines[len(lines)-1])
	}

	// Refuse to overwrite: replaced tokens could never be recovered.
	_, err = e.run("device", "register-synthetic", "--fleet", "lab-a", "--count", "1",
		"--prefix", "other-", "--platform", "linux/arm64", "--tokens-file", tokens)
	if err == nil {
		t.Fatal("overwrote an existing tokens file")
	}
}

func TestRegisterSyntheticSaysWhenTokensAreDiscarded(t *testing.T) {
	e := newEnv(t)
	e.mustRun("fleet", "create", "lab-a")
	out := e.mustRun("device", "register-synthetic", "--fleet", "lab-a", "--count", "2", "--platform", "linux/arm64")
	if strings.Contains(out, "frd_") {
		t.Fatalf("output contains tokens: %q", out)
	}
	if !strings.Contains(out, "--tokens-file") {
		t.Fatalf("output should mention how to keep tokens: %q", out)
	}
}

func TestArtifactPushSignsAndUploads(t *testing.T) {
	e := newEnv(t)
	content := []byte(strings.Repeat("hello artifact ", 500))
	file := filepath.Join(t.TempDir(), "a.bin")
	if err := os.WriteFile(file, content, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	args := []string{"artifact", "push", "--name", "demo", "--version", "1.0.0", "--platform", "linux/arm64",
		"--release", "1", "--file", file, "--key", e.key, "--chunk-size", "1024"}
	created := mustJSON[apiv1.Artifact](e, args...)

	digest := sha256.Sum256(content)
	if created.SHA256 != hex.EncodeToString(digest[:]) || created.SizeBytes != int64(len(content)) || !created.ContentReady {
		t.Fatalf("created = %+v", created)
	}
	if got := mustJSON[apiv1.Artifact](e, "artifact", "get", created.ID); got.ID != created.ID || !got.ContentReady {
		t.Fatalf("got = %+v", got)
	}

	// An interrupted push can simply be run again.
	if again := mustJSON[apiv1.Artifact](e, args...); again.ID != created.ID {
		t.Fatalf("a repeat returned %s, want %s", again.ID, created.ID)
	}
}

func TestArtifactPushIsRefusedWithAnUntrustedKey(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	e.mustRun("key", "generate", "--private-key", filepath.Join(dir, "other.key"), "--public-key", filepath.Join(dir, "other.pub"))
	file := filepath.Join(dir, "a.bin")
	if err := os.WriteFile(file, []byte(strings.Repeat("x", 2000)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := e.run("artifact", "push", "--name", "demo", "--version", "1.0.0", "--platform", "linux/arm64",
		"--release", "1", "--file", file, "--key", filepath.Join(dir, "other.key"), "--chunk-size", "1024")
	if err == nil || !strings.Contains(err.Error(), "trusted") {
		t.Fatalf("error = %v, want a refusal", err)
	}
}

func TestArtifactPushReportsLocalProblems(t *testing.T) {
	e := newEnv(t)
	dir := t.TempDir()
	base := []string{"artifact", "push", "--name", "demo", "--version", "1.0.0", "--platform", "linux/arm64", "--release", "1"}
	file := filepath.Join(dir, "a.bin")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	notAKey := filepath.Join(dir, "not-a-key")
	if err := os.WriteFile(notAKey, []byte("nope"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	for name, extra := range map[string][]string{
		"missing file": {"--file", filepath.Join(dir, "missing"), "--key", e.key},
		"missing key":  {"--file", file, "--key", filepath.Join(dir, "missing")},
		"bad key":      {"--file", file, "--key", notAKey},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := e.run(append(slices.Clone(base), extra...)...); err == nil {
				t.Fatal("push succeeded")
			}
		})
	}
}

func TestKeyGenerateWritesAUsablePairPrivately(t *testing.T) {
	var stdout, stderr bytes.Buffer
	dir := t.TempDir()
	private, public := filepath.Join(dir, "release.key"), filepath.Join(dir, "release.pub")
	args := []string{"key", "generate", "--private-key", private, "--public-key", public}
	// No server and no token are involved in making a key.
	if err := cli.Execute(t.Context(), args, &stdout, &stderr, func(string) string { return "" }); err != nil {
		t.Fatalf("generate: %v", err)
	}
	info, err := os.Stat(private)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("private key mode = %o", info.Mode().Perm())
	}
	encodedPrivate, err := os.ReadFile(private)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	key, err := signing.ParsePrivateKey(encodedPrivate)
	if err != nil {
		t.Fatalf("ParsePrivateKey: %v", err)
	}
	encodedPublic, err := os.ReadFile(public)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	set, err := signing.ParseTrustSet(encodedPublic)
	if err != nil {
		t.Fatalf("ParseTrustSet: %v", err)
	}
	_, envelope := signingtest.Signed(t, key, signing.Meta{
		Name: "demo", Version: "1.0.0", Platform: "linux/arm64", ReleaseCounter: 1, ChunkSize: 1024,
	}, []byte("content"))
	if _, err := set.Verify(envelope); err != nil {
		t.Fatalf("the pair doesn't match: %v", err)
	}
	if strings.Contains(stdout.String(), "PRIVATE") {
		t.Fatalf("printed key material: %s", stdout.String())
	}

	// Replacing a key by accident would strand every device that trusts it.
	if err := cli.Execute(t.Context(), args, &stdout, &stderr, func(string) string { return "" }); err == nil {
		t.Fatal("overwrote an existing key")
	}
}

func TestHelpDescribesEveryCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	getenv := func(string) string { return "" }
	if err := cli.Execute(t.Context(), []string{"--help"}, &stdout, &stderr, getenv); err != nil {
		t.Fatalf("help: %v", err)
	}
	for _, want := range []string{"key", "fleet", "device", "artifact", "rollout", "ROLLOUT_ADMIN_TOKEN", "ROLLOUT_API_URL"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("help is missing %q:\n%s", want, stdout.String())
		}
	}
	stdout.Reset()
	if err := cli.Execute(t.Context(), []string{"rollout", "--help"}, &stdout, &stderr, getenv); err != nil {
		t.Fatalf("rollout help: %v", err)
	}
	for _, want := range []string{"create", "status", "start", "pause", "resume", "abort", "cohort", "history"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("rollout help is missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestStatusExplainsProgressAndHalts(t *testing.T) {
	e := newEnv(t)
	e.mustRun("fleet", "create", "lab-a")
	file := filepath.Join(t.TempDir(), "a.bin")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	artifact := mustJSON[apiv1.Artifact](e, "artifact", "push", "--name", "demo", "--version", "1.0.0",
		"--platform", "linux/arm64", "--release", "1", "--file", file, "--key", e.key)
	var tokens []string
	for _, id := range []string{"bench-1", "bench-2"} {
		registered := mustJSON[apiv1.DeviceList](e, "device", "register", "--fleet", "lab-a", "--id", id, "--platform", "linux/arm64")
		tokens = append(tokens, registered.Devices[0].Token)
	}
	r := mustJSON[apiv1.Rollout](e, "rollout", "create", "--fleet", "lab-a", "--name", "demo", "--artifact", artifact.ID,
		"--stages", "100", "--max-failure-rate", "2%")
	e.mustRun("rollout", "start", r.ID)

	if status := e.mustRun("rollout", "status", r.ID); !strings.Contains(status, "Waiting:") || !strings.Contains(status, "coverage") {
		t.Fatalf("a running rollout should say what it's waiting for:\n%s", status)
	}

	// One device reports a failure, and the engine evaluates the rollout.
	device := client.NewDevice(e.base, tokens[0], nil)
	polled, err := device.Poll(t.Context())
	if err != nil || polled.Assignment == nil {
		t.Fatalf("Poll = %+v, err = %v", polled, err)
	}
	a := polled.Assignment
	if _, err := device.Report(t.Context(), apiv1.ReportRequest{
		Sequence: 1, RolloutID: a.RolloutID, AssignmentID: a.ID, Generation: a.Generation,
		State: rollout.AssignmentFailed, Reason: "health check failed",
	}); err != nil {
		t.Fatalf("Report: %v", err)
	}
	fence, ok, err := e.store.AcquireLease(t.Context(), "test", time.Minute)
	if err != nil || !ok {
		t.Fatalf("AcquireLease: ok = %v, err = %v", ok, err)
	}
	if _, err := e.store.AdvanceRollout(t.Context(), r.ID, fence); err != nil {
		t.Fatalf("AdvanceRollout: %v", err)
	}

	status := e.mustRun("rollout", "status", r.ID)
	for _, want := range []string{"halted", "Halted:", "stage_breach", "FAILED", "failed"} {
		if !strings.Contains(status, want) {
			t.Errorf("status is missing %q:\n%s", want, status)
		}
	}
	if strings.Contains(status, "Waiting:") {
		t.Errorf("a halted rollout isn't waiting for anything:\n%s", status)
	}
	history := e.mustRun("rollout", "history", r.ID)
	if !strings.Contains(history, "breach") || !strings.Contains(history, "engine") {
		t.Errorf("history doesn't show the engine's decision:\n%s", history)
	}
}

func TestARollbackFromTheCommandLine(t *testing.T) {
	e := newEnv(t)
	e.mustRun("fleet", "create", "lab-a")
	file := filepath.Join(t.TempDir(), "a.bin")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	artifact := mustJSON[apiv1.Artifact](e, "artifact", "push", "--name", "demo", "--version", "1.0.0",
		"--platform", "linux/arm64", "--release", "4", "--file", file, "--key", e.key)
	registered := mustJSON[apiv1.DeviceList](e, "device", "register", "--fleet", "lab-a", "--id", "bench-1",
		"--platform", "linux/arm64", "--installed-release", "3")
	r := mustJSON[apiv1.Rollout](e, "rollout", "create", "--fleet", "lab-a", "--name", "demo", "--artifact", artifact.ID,
		"--stages", "100", "--max-failure-rate", "2%", "--rollback-deadline", "20m")
	if got := r.Policy.Safety.RollbackDeadline.Std(); got != 20*time.Minute {
		t.Fatalf("rollback deadline = %v", got)
	}
	e.mustRun("rollout", "start", r.ID)

	// The device installs the release.
	device := client.NewDevice(e.base, registered.Devices[0].Token, nil)
	polled, err := device.Poll(t.Context())
	if err != nil || polled.Assignment == nil {
		t.Fatalf("Poll = %+v, err = %v", polled, err)
	}
	a := polled.Assignment
	for i, state := range []rollout.AssignmentState{rollout.AssignmentInstalling, rollout.AssignmentHealthCheck, rollout.AssignmentSucceeded} {
		if _, err := device.Report(t.Context(), apiv1.ReportRequest{
			Sequence: int64(i + 1), RolloutID: a.RolloutID, AssignmentID: a.ID, Generation: a.Generation, State: state,
		}); err != nil {
			t.Fatal(err)
		}
	}

	e.mustRun("rollout", "rollback", r.ID, "--reason", "customers report crashes")
	status := e.mustRun("rollout", "status", r.ID)
	if !strings.Contains(status, "rolling_back") || !strings.Contains(status, "attempt 1: 0 of 1 devices restored") {
		t.Fatalf("status doesn't show the rollback:\n%s", status)
	}
	polled, err = device.Poll(t.Context())
	if err != nil || polled.Assignment == nil || polled.Assignment.Kind != "rollback" ||
		polled.Assignment.RestoreRelease == nil || *polled.Assignment.RestoreRelease != 3 {
		t.Fatalf("Poll = %+v, err = %v; want a rollback to release 3", polled.Assignment, err)
	}
}
