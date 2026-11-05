package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Bispulm/fleet-rollout/internal/apiv1"
	"github.com/Bispulm/fleet-rollout/internal/signing/signingtest"
)

func writeTrust(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trust.pem")
	if err := os.WriteFile(path, signingtest.PublicPEM(t, signingtest.NewKey(t)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestRunValidatesItsConfiguration(t *testing.T) {
	trust := writeTrust(t)
	base := []string{"--api", "http://127.0.0.1:1", "--device-id", "sim-1", "--state-dir", t.TempDir(),
		"--platform", "linux/arm64", "--trust-keys", trust}
	withToken := func(k string) string {
		if k == "ROLLOUT_AGENT_TOKEN" {
			return "frd_token"
		}
		return ""
	}
	cases := []struct {
		name   string
		args   []string
		getenv func(string) string
		want   string
	}{
		{"no token", base, func(string) string { return "" }, "token"},
		{"no API", without(base, "--api"), withToken, "--api"},
		{"no device", without(base, "--device-id"), withToken, "--device-id"},
		{"no state", without(base, "--state-dir"), withToken, "--state-dir"},
		{"bad platform", replace(base, "--platform", "linux"), withToken, "--platform"},
		{"missing trust", replace(base, "--trust-keys", filepath.Join(t.TempDir(), "absent")), withToken, "trust"},
		{"unknown flag", append(base, "--surprise"), withToken, "surprise"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := run(t.Context(), tc.args, tc.getenv, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

func without(args []string, flag string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == flag {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func replace(args []string, flag, value string) []string {
	out := append([]string(nil), args...)
	for i := range out {
		if out[i] == flag {
			out[i+1] = value
		}
	}
	return out
}

func TestRunPollsWithItsTokenUntilStoppedAndKeepsState(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/device/poll" || r.Header.Get("Authorization") != "Bearer frd_from_file" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		polls.Add(1)
		_ = json.NewEncoder(w).Encode(apiv1.PollResponse{PollAfterSeconds: 1})
	}))
	defer srv.Close()

	stateDir := t.TempDir()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("frd_from_file\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"--api", srv.URL, "--device-id", "sim-1", "--token-file", tokenFile,
			"--state-dir", stateDir, "--platform", "linux/arm64", "--trust-keys", writeTrust(t)},
			func(string) string { return "" }, io.Discard)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for polls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
	if polls.Load() == 0 {
		t.Fatal("it never polled")
	}
	if _, err := os.Stat(filepath.Join(stateDir, "checkpoint.json")); err != nil {
		t.Fatalf("no checkpoint was written: %v", err)
	}
}
