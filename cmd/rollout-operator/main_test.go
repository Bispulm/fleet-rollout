package main

import (
	"bytes"
	"testing"
)

func TestFlagsAreChecked(t *testing.T) {
	withToken := func(name string) string {
		if name == adminTokenEnv {
			return "token"
		}
		return ""
	}
	for name, tc := range map[string]struct {
		args   []string
		getenv func(string) string
	}{
		"no token":      {nil, func(string) string { return "" }},
		"not a URL":     {[]string{"--api", "rolloutd:18100"}, withToken},
		"no host":       {[]string{"--api", "http://"}, withToken},
		"bad log level": {[]string{"--log-level", "loud"}, withToken},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parse(tc.args, tc.getenv, &bytes.Buffer{}); err == nil {
				t.Fatalf("accepted %v", tc.args)
			}
		})
	}

	cfg, err := parse([]string{"--api", "http://rolloutd:18100", "--leader-elect"}, withToken, &bytes.Buffer{})
	if err != nil || cfg.apiURL != "http://rolloutd:18100" || !cfg.leaderElect || cfg.adminToken != "token" {
		t.Fatalf("config = %+v, %v", cfg, err)
	}
}
