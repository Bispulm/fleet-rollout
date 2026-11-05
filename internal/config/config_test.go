package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(overrides map[string]string) func(string) string {
	base := map[string]string{
		"ROLLOUTD_DATABASE_URL": "postgres://u:hunter2@127.0.0.1:15431/rollout",
		"ROLLOUTD_ADMIN_TOKEN":  strings.Repeat("x", minAdminTokenLength),
	}
	for k, v := range overrides {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}

func TestLoadAppliesLoopbackAndDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:18100" {
		t.Errorf("ListenAddr = %q, want the loopback default", cfg.ListenAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 15s", cfg.ShutdownTimeout)
	}
	if cfg.DBMaxConns != 16 {
		t.Errorf("DBMaxConns = %d, want 16", cfg.DBMaxConns)
	}
}

func TestLoadRejectsMissingSecrets(t *testing.T) {
	for _, name := range []string{"ROLLOUTD_DATABASE_URL", "ROLLOUTD_ADMIN_TOKEN"} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(map[string]string{name: ""}))
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("Load error = %v, want one naming %s", err, name)
			}
		})
	}
}

func TestLoadRejectsShortAdminToken(t *testing.T) {
	_, err := Load(env(map[string]string{"ROLLOUTD_ADMIN_TOKEN": "short"}))
	if err == nil || !strings.Contains(err.Error(), "ROLLOUTD_ADMIN_TOKEN") {
		t.Fatalf("Load error = %v, want one naming the token variable", err)
	}
}

func TestLoadNeverEchoesSecretsInErrors(t *testing.T) {
	_, err := Load(env(map[string]string{
		"ROLLOUTD_DATABASE_URL": "not a url hunter2",
		"ROLLOUTD_ADMIN_TOKEN":  "short-secret",
	}))
	if err == nil {
		t.Fatal("Load succeeded, want an error")
	}
	for _, secret := range []string{"hunter2", "short-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error %q leaks %q", err, secret)
		}
	}
}

func TestLoadListenAddress(t *testing.T) {
	cases := []struct {
		name    string
		addr    string
		allow   string
		wantErr bool
	}{
		{name: "loopback v4", addr: "127.0.0.1:18101"},
		{name: "loopback v6", addr: "[::1]:18101"},
		{name: "localhost", addr: "localhost:18101"},
		{name: "wildcard refused", addr: ":18101", wantErr: true},
		{name: "routable refused", addr: "10.0.0.5:18101", wantErr: true},
		{name: "wildcard explicitly allowed", addr: ":18101", allow: "true"},
		{name: "malformed", addr: "no-port", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(map[string]string{
				"ROLLOUTD_LISTEN_ADDR":        tc.addr,
				"ROLLOUTD_ALLOW_NON_LOOPBACK": tc.allow,
			}))
			if (err != nil) != tc.wantErr {
				t.Fatalf("Load(%q) error = %v, wantErr %v", tc.addr, err, tc.wantErr)
			}
		})
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	cases := map[string]string{
		"ROLLOUTD_LOG_LEVEL":        "shouty",
		"ROLLOUTD_SHUTDOWN_TIMEOUT": "soon",
		"ROLLOUTD_DB_MAX_CONNS":     "0",
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(env(map[string]string{name: value}))
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("Load error = %v, want one naming %s", err, name)
			}
		})
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Load(func(string) string { return "" })
	if err == nil {
		t.Fatal("Load succeeded, want an error")
	}
	for _, name := range []string{"ROLLOUTD_DATABASE_URL", "ROLLOUTD_ADMIN_TOKEN"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not mention %s", err, name)
		}
	}
}
