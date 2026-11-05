package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/berkheev/tollgate/internal/ratelimit"
)

const sample = `
listen: ":9999"
backend: memory
failure_mode: closed
backend_timeout: 250ms
redis:
  url: redis://cache:6379/2
rules:
  api_per_user: token_bucket 100/min burst 20
  login_attempts:
    algorithm: sliding_log
    limit: 5
    window: 15m
`

func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tollgate.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func noEnv(string) string { return "" }

func TestLoadFile(t *testing.T) {
	cfg, err := Load(writeFile(t, sample), noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9999" || cfg.Backend != BackendMemory || cfg.FailOpen() {
		t.Fatalf("unexpected config %+v", cfg)
	}
	if cfg.BackendTimeout != 250*time.Millisecond {
		t.Fatalf("backend_timeout = %s", cfg.BackendTimeout)
	}
	if cfg.Redis.KeyPrefix != "tollgate" {
		t.Fatalf("default key prefix lost: %q", cfg.Redis.KeyPrefix)
	}

	rs, err := cfg.RuleSet()
	if err != nil {
		t.Fatal(err)
	}
	login, ok := rs.Get("login_attempts")
	if !ok {
		t.Fatal("login_attempts missing")
	}
	want := ratelimit.Rule{Name: "login_attempts", Algorithm: ratelimit.SlidingLog, Limit: 5, Window: 15 * time.Minute}
	if login != want {
		t.Fatalf("got %+v, want %+v", login, want)
	}
	api, _ := rs.Get("api_per_user")
	if api.Burst != 20 || api.Limit != 100 || api.Window != time.Minute {
		t.Fatalf("api_per_user parsed as %+v", api)
	}
}

func TestEnvOverridesFile(t *testing.T) {
	env := map[string]string{
		"TOLLGATE_BACKEND":         "redis",
		"TOLLGATE_REDIS_URL":       "redis://other:6379/0",
		"TOLLGATE_FAILURE_MODE":    "open",
		"TOLLGATE_BACKEND_TIMEOUT": "40ms",
		"TOLLGATE_LOG_LEVEL":       "debug",
	}
	cfg, err := Load(writeFile(t, sample), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backend != BackendRedis || cfg.Redis.URL != "redis://other:6379/0" || !cfg.FailOpen() {
		t.Fatalf("env not applied: %+v", cfg)
	}
	if cfg.BackendTimeout != 40*time.Millisecond {
		t.Fatalf("backend_timeout = %s", cfg.BackendTimeout)
	}
	if lvl, _ := cfg.SlogLevel(); lvl.String() != "DEBUG" {
		t.Fatalf("level = %s", lvl)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
		env     map[string]string
		want    string
	}{
		{name: "no rules", content: "backend: memory\n", want: "no rules"},
		{name: "bad backend", content: "backend: etcd\nrules:\n  a: fixed_window 1/s\n", want: "backend must be"},
		{name: "bad failure mode", content: "failure_mode: maybe\nrules:\n  a: fixed_window 1/s\n", want: "failure_mode"},
		{name: "bad rule", content: "rules:\n  a: fixed_window 1\n", want: "line 2"},
		{name: "bad rule mapping", content: "rules:\n  a:\n    algorithm: fixed_window\n    limit: 1\n    window: soon\n", want: "invalid window"},
		{name: "rules not a mapping", content: "rules:\n  - a\n", want: "mapping"},
		{name: "bad env duration", content: "rules:\n  a: fixed_window 1/s\n", env: map[string]string{"TOLLGATE_BACKEND_TIMEOUT": "fast"}, want: "TOLLGATE_BACKEND_TIMEOUT"},
		{name: "bad log level", content: "log:\n  level: loud\nrules:\n  a: fixed_window 1/s\n", want: "log.level"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeFile(t, tt.content), func(k string) string { return tt.env[k] })
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("want error containing %q, got %v", tt.want, err)
			}
		})
	}
}

func TestShippedConfigIsValid(t *testing.T) {
	cfg, err := Load("../../config/tollgate.yaml", noEnv)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cfg.RuleSet(); err != nil {
		t.Fatal(err)
	}
}
