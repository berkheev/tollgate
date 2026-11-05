package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/redis/go-redis/v9"

	"github.com/berkheev/tollgate/internal/clock"
	"github.com/berkheev/tollgate/internal/config"
	"github.com/berkheev/tollgate/internal/ratelimit"
	"github.com/berkheev/tollgate/internal/server"
)

var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}

	configPath := flag.String("config", os.Getenv("TOLLGATE_CONFIG"), "path to the YAML config file (env TOLLGATE_CONFIG)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("tollgate", version, runtime.Version())
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, *configPath)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tollgate:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, configPath string) error {
	cfg, err := config.Load(configPath, os.Getenv)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	log, err := newLogger(cfg, os.Stdout)
	if err != nil {
		return err
	}
	rules, err := cfg.RuleSet()
	if err != nil {
		return fmt.Errorf("rules: %w", err)
	}

	backend, err := newBackend(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer func() { _ = backend.Close() }()

	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo(),
	)

	srv := server.New(server.Options{
		Backend:        backend,
		Rules:          rules,
		FailOpen:       cfg.FailOpen(),
		BackendTimeout: cfg.BackendTimeout,
		Logger:         log,
		Registerer:     reg,
		Gatherer:       reg,
	})

	go reloadOnHUP(ctx, configPath, srv, log)

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return err
	}
	log.Info("tollgate started",
		"version", version,
		"addr", ln.Addr().String(),
		"backend", cfg.Backend,
		"failure_mode", cfg.FailureMode,
		"rules", rules.Len(),
	)

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down", "timeout", cfg.ShutdownTimeout.String())
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errCh; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	log.Info("stopped")
	return nil
}

func newLogger(cfg config.Config, w io.Writer) (*slog.Logger, error) {
	level, err := cfg.SlogLevel()
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}
	if cfg.Log.Format == "text" {
		return slog.New(slog.NewTextHandler(w, opts)), nil
	}
	return slog.New(slog.NewJSONHandler(w, opts)), nil
}

func newBackend(ctx context.Context, cfg config.Config, log *slog.Logger) (ratelimit.Backend, error) {
	if cfg.Backend == config.BackendMemory {
		m := ratelimit.NewMemory(clock.Real{})
		go m.RunSweeper(ctx, time.Minute)
		return m, nil
	}

	opts, err := redis.ParseURL(cfg.Redis.URL)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	opts.ContextTimeoutEnabled = true
	if cfg.Redis.PoolSize > 0 {
		opts.PoolSize = cfg.Redis.PoolSize
	}
	b := ratelimit.NewRedis(redis.NewClient(opts), clock.Real{}, cfg.Redis.KeyPrefix)

	// Not fatal: in fail-open mode tollgate should come up even if Redis is
	// still starting. Scripts are loaded lazily on first use anyway.
	startCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := b.LoadScripts(startCtx); err != nil {
		log.Warn("redis not ready at startup", "addr", opts.Addr, "err", err)
	}
	return b, nil
}

func reloadOnHUP(ctx context.Context, path string, srv *server.Server, log *slog.Logger) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
		}
		cfg, err := config.Load(path, os.Getenv)
		if err == nil {
			var rules *ratelimit.RuleSet
			if rules, err = cfg.RuleSet(); err == nil {
				srv.SetRules(rules)
				log.Info("rules reloaded", "rules", rules.Len())
				continue
			}
		}
		log.Error("reload failed, keeping current rules", "err", err)
	}
}

func buildInfo() prometheus.Collector {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "tollgate_build_info",
		Help: "Build information.",
	}, []string{"version", "goversion"})
	g.WithLabelValues(version, runtime.Version()).Set(1)
	return g
}

// healthcheck backs the Docker HEALTHCHECK: the distroless image has no curl.
func healthcheck() int {
	addr := os.Getenv("TOLLGATE_LISTEN")
	if addr == "" {
		addr = ":8090"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := http.Client{Timeout: 2 * time.Second}
	url := "http://" + net.JoinHostPort(host, port) + "/healthz"
	// The URL is loopback plus our own listen port, not user input.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil) //nolint:gosec
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp, err := client.Do(req) //nolint:gosec
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "status", resp.StatusCode)
		return 1
	}
	return 0
}
