package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(overrides map[string]string) func(string) string {
	base := map[string]string{
		"ROLLOUTD_DATABASE_URL":    "postgres://u:hunter2@127.0.0.1:15431/rollout",
		"ROLLOUTD_ADMIN_TOKEN":     strings.Repeat("x", minAdminTokenLength),
		"ROLLOUTD_TRUST_KEYS_FILE": "/etc/rollout/trust.pem",
		"ROLLOUTD_ARTIFACT_DIR":    "/var/lib/rollout/artifacts",
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
	for _, name := range []string{"ROLLOUTD_DATABASE_URL", "ROLLOUTD_ADMIN_TOKEN", "ROLLOUTD_TRUST_KEYS_FILE", "ROLLOUTD_ARTIFACT_DIR"} {
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

func TestLoadEngineAndDeviceTimingDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.EngineEnabled {
		t.Error("the engine should run by default")
	}
	if cfg.EngineInterval != 5*time.Second || cfg.EngineLeaseTTL != 30*time.Second {
		t.Errorf("engine timing = %v, %v", cfg.EngineInterval, cfg.EngineLeaseTTL)
	}
	if cfg.PollInterval != time.Minute {
		t.Errorf("poll interval = %v", cfg.PollInterval)
	}
}

func TestLoadEngineCanBeDisabled(t *testing.T) {
	cfg, err := Load(env(map[string]string{"ROLLOUTD_ENGINE_ENABLED": "false"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EngineEnabled {
		t.Fatal("the engine is still enabled")
	}
}

func TestLoadRejectsUnsafeTiming(t *testing.T) {
	cases := []struct {
		name string
		vars map[string]string
		want string
	}{
		{"unparseable interval", map[string]string{"ROLLOUTD_ENGINE_INTERVAL": "soon"}, "ROLLOUTD_ENGINE_INTERVAL"},
		{"zero interval", map[string]string{"ROLLOUTD_ENGINE_INTERVAL": "0s"}, "ROLLOUTD_ENGINE_INTERVAL"},
		{"unparseable TTL", map[string]string{"ROLLOUTD_ENGINE_LEASE_TTL": "soon"}, "ROLLOUTD_ENGINE_LEASE_TTL"},
		{"TTL shorter than three intervals", map[string]string{"ROLLOUTD_ENGINE_INTERVAL": "10s", "ROLLOUTD_ENGINE_LEASE_TTL": "29s"}, "ROLLOUTD_ENGINE_LEASE_TTL"},
		{"poll interval under a second", map[string]string{"ROLLOUTD_POLL_INTERVAL": "500ms"}, "ROLLOUTD_POLL_INTERVAL"},
		{"bad boolean", map[string]string{"ROLLOUTD_ENGINE_ENABLED": "maybe"}, "ROLLOUTD_ENGINE_ENABLED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(env(tc.vars))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load error = %v, want one naming %s", err, tc.want)
			}
		})
	}
}

func TestLoadAcceptsTimingAtTheBoundaries(t *testing.T) {
	_, err := Load(env(map[string]string{
		"ROLLOUTD_ENGINE_INTERVAL":  "10s",
		"ROLLOUTD_ENGINE_LEASE_TTL": "30s",
		"ROLLOUTD_POLL_INTERVAL":    "1s",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
}

func TestLoadArtifactLimit(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxArtifactBytes != 1<<30 {
		t.Fatalf("default limit = %d", cfg.MaxArtifactBytes)
	}
	cfg, err = Load(env(map[string]string{"ROLLOUTD_MAX_ARTIFACT_BYTES": "2048"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxArtifactBytes != 2048 || cfg.TrustKeysFile != "/etc/rollout/trust.pem" || cfg.ArtifactDir != "/var/lib/rollout/artifacts" {
		t.Fatalf("config = %+v", cfg)
	}
	for _, bad := range []string{"0", "-1", "lots"} {
		if _, err := Load(env(map[string]string{"ROLLOUTD_MAX_ARTIFACT_BYTES": bad})); err == nil {
			t.Errorf("accepted a limit of %q", bad)
		}
	}
}

func TestLoadDeviceLimits(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil || cfg.MaxDeviceRequests != 256 || cfg.MaxDeviceDownloads != 128 {
		t.Fatalf("defaults = %d, %d, err = %v", cfg.MaxDeviceRequests, cfg.MaxDeviceDownloads, err)
	}
	cfg, err = Load(env(map[string]string{"ROLLOUTD_MAX_DEVICE_REQUESTS": "32", "ROLLOUTD_MAX_DEVICE_DOWNLOADS": "8"}))
	if err != nil || cfg.MaxDeviceRequests != 32 || cfg.MaxDeviceDownloads != 8 {
		t.Fatalf("configured = %d, %d, err = %v", cfg.MaxDeviceRequests, cfg.MaxDeviceDownloads, err)
	}
	for _, name := range []string{"ROLLOUTD_MAX_DEVICE_REQUESTS", "ROLLOUTD_MAX_DEVICE_DOWNLOADS"} {
		for _, bad := range []string{"0", "-5", "many"} {
			if _, err := Load(env(map[string]string{name: bad})); err == nil {
				t.Errorf("%s: accepted %q", name, bad)
			}
		}
	}
}

func TestLoadMetricsAddress(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil || cfg.MetricsAddr != "127.0.0.1:18102" {
		t.Fatalf("default = %q, %v", cfg.MetricsAddr, err)
	}
	if cfg, err = Load(env(map[string]string{"ROLLOUTD_METRICS_ADDR": "off"})); err != nil || cfg.MetricsAddr != "" {
		t.Fatalf("off = %q, %v", cfg.MetricsAddr, err)
	}
	if _, err = Load(env(map[string]string{"ROLLOUTD_METRICS_ADDR": "0.0.0.0:18102"})); err == nil ||
		!strings.Contains(err.Error(), "ROLLOUTD_METRICS_ADDR") {
		t.Fatalf("a non-loopback address without permission: error = %v", err)
	}
	cfg, err = Load(env(map[string]string{"ROLLOUTD_METRICS_ADDR": "0.0.0.0:18102", "ROLLOUTD_ALLOW_NON_LOOPBACK": "true"}))
	if err != nil || cfg.MetricsAddr != "0.0.0.0:18102" {
		t.Fatalf("with permission = %q, %v", cfg.MetricsAddr, err)
	}
}
