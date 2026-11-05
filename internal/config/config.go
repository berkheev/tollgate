package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/berkheev/tollgate/internal/ratelimit"
)

type Config struct {
	Listen          string        `yaml:"listen"`
	Backend         string        `yaml:"backend"`
	FailureMode     string        `yaml:"failure_mode"`
	BackendTimeout  time.Duration `yaml:"backend_timeout"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout"`
	Redis           Redis         `yaml:"redis"`
	Log             Log           `yaml:"log"`
	Rules           Rules         `yaml:"rules"`
}

type Redis struct {
	URL       string `yaml:"url"`
	KeyPrefix string `yaml:"key_prefix"`
	PoolSize  int    `yaml:"pool_size"`
}

type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

const (
	BackendRedis  = "redis"
	BackendMemory = "memory"

	FailOpen   = "open"
	FailClosed = "closed"
)

func Default() Config {
	return Config{
		Listen:          ":8090",
		Backend:         BackendRedis,
		FailureMode:     FailOpen,
		BackendTimeout:  100 * time.Millisecond,
		ShutdownTimeout: 15 * time.Second,
		Redis: Redis{
			URL:       "redis://localhost:6379/0",
			KeyPrefix: "tollgate",
		},
		Log: Log{Level: "info", Format: "json"},
	}
}

// Load reads the YAML file at path (if any) on top of the defaults and then
// applies TOLLGATE_* environment variables, which win over the file.
func Load(path string, getenv func(string) string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return Config{}, err
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", path, err)
		}
	}
	if err := cfg.applyEnv(getenv); err != nil {
		return Config{}, err
	}
	return cfg, cfg.Validate()
}

func (c *Config) applyEnv(getenv func(string) string) error {
	strs := map[string]*string{
		"TOLLGATE_LISTEN":       &c.Listen,
		"TOLLGATE_BACKEND":      &c.Backend,
		"TOLLGATE_FAILURE_MODE": &c.FailureMode,
		"TOLLGATE_REDIS_URL":    &c.Redis.URL,
		"TOLLGATE_KEY_PREFIX":   &c.Redis.KeyPrefix,
		"TOLLGATE_LOG_LEVEL":    &c.Log.Level,
		"TOLLGATE_LOG_FORMAT":   &c.Log.Format,
	}
	for name, dst := range strs {
		if v := getenv(name); v != "" {
			*dst = v
		}
	}

	durations := map[string]*time.Duration{
		"TOLLGATE_BACKEND_TIMEOUT":  &c.BackendTimeout,
		"TOLLGATE_SHUTDOWN_TIMEOUT": &c.ShutdownTimeout,
	}
	for name, dst := range durations {
		if v := getenv(name); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			*dst = d
		}
	}
	return nil
}

func (c Config) Validate() error {
	var errs []error
	if c.Backend != BackendRedis && c.Backend != BackendMemory {
		errs = append(errs, fmt.Errorf("backend must be %q or %q, got %q", BackendRedis, BackendMemory, c.Backend))
	}
	if c.FailureMode != FailOpen && c.FailureMode != FailClosed {
		errs = append(errs, fmt.Errorf("failure_mode must be %q or %q, got %q", FailOpen, FailClosed, c.FailureMode))
	}
	if c.BackendTimeout <= 0 {
		errs = append(errs, errors.New("backend_timeout must be positive"))
	}
	if _, err := c.SlogLevel(); err != nil {
		errs = append(errs, err)
	}
	if c.Log.Format != "json" && c.Log.Format != "text" {
		errs = append(errs, fmt.Errorf("log.format must be json or text, got %q", c.Log.Format))
	}
	if len(c.Rules) == 0 {
		errs = append(errs, errors.New("no rules configured"))
	}
	return errors.Join(errs...)
}

func (c Config) FailOpen() bool { return c.FailureMode == FailOpen }

func (c Config) SlogLevel() (slog.Level, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(strings.ToUpper(c.Log.Level))); err != nil {
		return 0, fmt.Errorf("log.level: %w", err)
	}
	return l, nil
}

func (c Config) RuleSet() (*ratelimit.RuleSet, error) {
	return ratelimit.NewRuleSet(c.Rules...)
}

// Rules accepts both the one-line form and an explicit mapping:
//
//	rules:
//	  api_per_user: token_bucket 100/min burst 20
//	  login_attempts:
//	    algorithm: sliding_log
//	    limit: 5
//	    window: 15m
type Rules []ratelimit.Rule

type ruleMapping struct {
	Algorithm ratelimit.Algorithm `yaml:"algorithm"`
	Limit     int64               `yaml:"limit"`
	Window    string              `yaml:"window"`
	Burst     int64               `yaml:"burst"`
}

func (rs *Rules) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: rules must be a mapping of name to rule", node.Line)
	}
	out := make(Rules, 0, len(node.Content)/2)
	var errs []error
	for i := 0; i < len(node.Content); i += 2 {
		name, value := node.Content[i].Value, node.Content[i+1]
		r, err := decodeRule(name, value)
		if err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", value.Line, err))
			continue
		}
		out = append(out, r)
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	*rs = out
	return nil
}

func decodeRule(name string, node *yaml.Node) (ratelimit.Rule, error) {
	switch node.Kind {
	case yaml.ScalarNode:
		return ratelimit.ParseRule(name, node.Value)
	case yaml.MappingNode:
		var m ruleMapping
		if err := node.Decode(&m); err != nil {
			return ratelimit.Rule{}, fmt.Errorf("rule %s: %w", name, err)
		}
		window, err := ratelimit.ParseWindow(m.Window)
		if err != nil {
			return ratelimit.Rule{}, fmt.Errorf("rule %s: %w", name, err)
		}
		r := ratelimit.Rule{Name: name, Algorithm: m.Algorithm, Limit: m.Limit, Window: window, Burst: m.Burst}
		return r, r.Validate()
	}
	return ratelimit.Rule{}, fmt.Errorf("rule %s: expected a string or a mapping", name)
}
