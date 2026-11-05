// Package config loads rolloutd settings from the environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"
)

const (
	defaultListenAddr      = "127.0.0.1:18100"
	defaultShutdownTimeout = 15 * time.Second
	defaultDBMaxConns      = 16
	minAdminTokenLength    = 16
	defaultEngineInterval  = 5 * time.Second
	defaultEngineLeaseTTL  = 30 * time.Second
	defaultPollInterval    = time.Minute
	defaultAssignmentLease = 5 * time.Minute
	// A slow tick or two shouldn't cost the engine its lease.
	minLeaseTTLIntervals = 3
	// A device that misses one poll shouldn't lose its assignment's lease.
	minLeasePollIntervals = 2
)

// Config holds the settings for the rolloutd server.
type Config struct {
	ListenAddr      string
	DatabaseURL     string
	AdminToken      string
	LogLevel        slog.Level
	ShutdownTimeout time.Duration
	DBMaxConns      int32

	EngineEnabled   bool
	EngineInterval  time.Duration
	EngineLeaseTTL  time.Duration
	PollInterval    time.Duration
	AssignmentLease time.Duration
}

// Load reads settings through getenv, which is normally os.Getenv. It reports
// every problem at once and never includes secret values in its errors.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		ListenAddr:      defaultListenAddr,
		LogLevel:        slog.LevelInfo,
		ShutdownTimeout: defaultShutdownTimeout,
		DBMaxConns:      defaultDBMaxConns,
		EngineEnabled:   true,
		EngineInterval:  defaultEngineInterval,
		EngineLeaseTTL:  defaultEngineLeaseTTL,
		PollInterval:    defaultPollInterval,
		AssignmentLease: defaultAssignmentLease,
	}
	var problems []error

	cfg.DatabaseURL = getenv("ROLLOUTD_DATABASE_URL")
	switch {
	case cfg.DatabaseURL == "":
		problems = append(problems, errors.New("ROLLOUTD_DATABASE_URL is required"))
	case !strings.HasPrefix(cfg.DatabaseURL, "postgres://") && !strings.HasPrefix(cfg.DatabaseURL, "postgresql://"):
		problems = append(problems, errors.New("ROLLOUTD_DATABASE_URL must be a postgres:// URL"))
	}

	cfg.AdminToken = getenv("ROLLOUTD_ADMIN_TOKEN")
	switch {
	case cfg.AdminToken == "":
		problems = append(problems, errors.New("ROLLOUTD_ADMIN_TOKEN is required"))
	case len(cfg.AdminToken) < minAdminTokenLength:
		problems = append(problems, fmt.Errorf("ROLLOUTD_ADMIN_TOKEN must be at least %d characters", minAdminTokenLength))
	}

	if v := getenv("ROLLOUTD_LISTEN_ADDR"); v != "" {
		cfg.ListenAddr = v
	}
	allowNonLoopback := getenv("ROLLOUTD_ALLOW_NON_LOOPBACK") == "true"
	if err := checkListenAddr(cfg.ListenAddr, allowNonLoopback); err != nil {
		problems = append(problems, err)
	}

	if v := getenv("ROLLOUTD_LOG_LEVEL"); v != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(v)); err != nil {
			problems = append(problems, fmt.Errorf("ROLLOUTD_LOG_LEVEL: unknown level %q", v))
		}
	}

	if v := getenv("ROLLOUTD_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			problems = append(problems, fmt.Errorf("ROLLOUTD_SHUTDOWN_TIMEOUT: %q is not a positive duration", v))
		} else {
			cfg.ShutdownTimeout = d
		}
	}

	if v := getenv("ROLLOUTD_DB_MAX_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 1 {
			problems = append(problems, fmt.Errorf("ROLLOUTD_DB_MAX_CONNS: %q is not a positive integer", v))
		} else {
			cfg.DBMaxConns = int32(n)
		}
	}

	problems = append(problems, cfg.loadTiming(getenv)...)

	if err := errors.Join(problems...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration:\n%w", err)
	}
	return cfg, nil
}

// loadTiming reads the engine and device timers, and checks them against each other.
func (cfg *Config) loadTiming(getenv func(string) string) []error {
	var problems []error
	if v := getenv("ROLLOUTD_ENGINE_ENABLED"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			problems = append(problems, fmt.Errorf("ROLLOUTD_ENGINE_ENABLED: %q is not a boolean", v))
		}
		cfg.EngineEnabled = enabled
	}
	durations := []struct {
		name string
		dst  *time.Duration
	}{
		{"ROLLOUTD_ENGINE_INTERVAL", &cfg.EngineInterval},
		{"ROLLOUTD_ENGINE_LEASE_TTL", &cfg.EngineLeaseTTL},
		{"ROLLOUTD_POLL_INTERVAL", &cfg.PollInterval},
		{"ROLLOUTD_ASSIGNMENT_LEASE", &cfg.AssignmentLease},
	}
	for _, d := range durations {
		v := getenv(d.name)
		if v == "" {
			continue
		}
		parsed, err := time.ParseDuration(v)
		if err != nil || parsed <= 0 {
			problems = append(problems, fmt.Errorf("%s: %q is not a positive duration", d.name, v))
			continue
		}
		*d.dst = parsed
	}
	if cfg.EngineLeaseTTL < minLeaseTTLIntervals*cfg.EngineInterval {
		problems = append(problems, fmt.Errorf("ROLLOUTD_ENGINE_LEASE_TTL must be at least %d times ROLLOUTD_ENGINE_INTERVAL", minLeaseTTLIntervals))
	}
	if cfg.PollInterval < time.Second {
		problems = append(problems, errors.New("ROLLOUTD_POLL_INTERVAL must be at least 1s"))
	}
	if cfg.AssignmentLease < minLeasePollIntervals*cfg.PollInterval {
		problems = append(problems, fmt.Errorf("ROLLOUTD_ASSIGNMENT_LEASE must be at least %d times ROLLOUTD_POLL_INTERVAL", minLeasePollIntervals))
	}
	return problems
}

// checkListenAddr refuses addresses reachable from other machines unless the
// operator opts in, because the server does not terminate TLS itself.
func checkListenAddr(addr string, allowNonLoopback bool) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("ROLLOUTD_LISTEN_ADDR: %q is not host:port", addr)
	}
	if allowNonLoopback || host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("ROLLOUTD_LISTEN_ADDR: %q is not a loopback address; set ROLLOUTD_ALLOW_NON_LOOPBACK=true to allow it", addr)
}
