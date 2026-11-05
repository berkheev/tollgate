package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/berkheev/tollgate/internal/ratelimit"
)

const (
	maxBodyBytes = 16 << 10
	maxKeyLength = 512
)

type Options struct {
	Backend        ratelimit.Backend
	Rules          *ratelimit.RuleSet
	FailOpen       bool
	BackendTimeout time.Duration
	Logger         *slog.Logger
	Registerer     prometheus.Registerer
	Gatherer       prometheus.Gatherer
}

type Server struct {
	backend  ratelimit.Backend
	rules    atomic.Pointer[ratelimit.RuleSet]
	failOpen bool
	timeout  time.Duration
	log      *slog.Logger
	metrics  *metrics
	gatherer prometheus.Gatherer
	errLog   sampler
}

func New(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Registerer == nil {
		reg := prometheus.NewRegistry()
		o.Registerer, o.Gatherer = reg, reg
	}
	if o.BackendTimeout <= 0 {
		o.BackendTimeout = 100 * time.Millisecond
	}
	s := &Server{
		backend:  o.Backend,
		failOpen: o.FailOpen,
		timeout:  o.BackendTimeout,
		log:      o.Logger,
		metrics:  newMetrics(o.Registerer),
		gatherer: o.Gatherer,
		errLog:   sampler{every: time.Second},
	}
	s.SetRules(o.Rules)
	return s
}

// SetRules swaps the rule set atomically; in-flight requests keep the rules
// they started with.
func (s *Server) SetRules(rs *ratelimit.RuleSet) {
	s.rules.Store(rs)
	s.metrics.rules.Set(float64(rs.Len()))
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/check", s.handleCheck)
	mux.HandleFunc("POST /v1/reset", s.handleReset)
	mux.HandleFunc("GET /v1/rules", s.handleRules)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	if s.gatherer != nil {
		mux.Handle("GET /metrics", promhttp.HandlerFor(s.gatherer, promhttp.HandlerOpts{}))
	}
	return s.instrument(mux)
}

type checkRequest struct {
	Rule string `json:"rule"`
	Key  string `json:"key"`
	Cost *int64 `json:"cost,omitempty"`
}

type checkResponse struct {
	Allowed      bool  `json:"allowed"`
	Limit        int64 `json:"limit"`
	Remaining    int64 `json:"remaining"`
	ResetAfterMs int64 `json:"reset_after_ms"`
	RetryAfterMs int64 `json:"retry_after_ms"`
	Degraded     bool  `json:"degraded,omitempty"`
}

func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
	var req checkRequest
	if !decode(w, r, &req) {
		return
	}
	rule, ok := s.lookup(w, req.Rule, req.Key)
	if !ok {
		return
	}
	cost := int64(1)
	if req.Cost != nil {
		cost = *req.Cost
	}
	if cost < 1 || cost > rule.Capacity() {
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("cost must be between 1 and %d for rule %s", rule.Capacity(), rule.Name))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	start := time.Now()
	res, err := s.backend.Check(ctx, rule, req.Key, cost)
	s.metrics.checkDuration.WithLabelValues(rule.Name).Observe(time.Since(start).Seconds())
	cancel()

	degraded := false
	if err != nil {
		s.metrics.backendErrors.WithLabelValues("check").Inc()
		if s.errLog.ok() {
			s.log.Error("backend check failed", "rule", rule.Name, "fail_open", s.failOpen, "err", err)
		}
		degraded = true
		res = ratelimit.Result{Allowed: s.failOpen, Limit: rule.Capacity()}
		if !s.failOpen {
			res.RetryAfter = time.Second
		}
	}

	decision := "allowed"
	status := http.StatusOK
	if !res.Allowed {
		decision, status = "denied", http.StatusTooManyRequests
	}
	s.metrics.decisions.WithLabelValues(rule.Name, decision).Inc()

	h := w.Header()
	if degraded {
		h.Set("Tollgate-Degraded", "true")
	} else {
		setRateLimitHeaders(h, rule, res)
	}
	if !res.Allowed {
		h.Set("Retry-After", strconv.FormatInt(max(seconds(res.RetryAfter), 1), 10))
	}

	writeJSON(w, status, checkResponse{
		Allowed:      res.Allowed,
		Limit:        res.Limit,
		Remaining:    res.Remaining,
		ResetAfterMs: res.ResetAfter.Milliseconds(),
		RetryAfterMs: res.RetryAfter.Milliseconds(),
		Degraded:     degraded,
	})
}

func setRateLimitHeaders(h http.Header, rule ratelimit.Rule, res ratelimit.Result) {
	policy := fmt.Sprintf("%d;w=%d", rule.Limit, max(seconds(rule.Window), 1))
	if rule.Burst > 0 {
		policy += ";burst=" + strconv.FormatInt(rule.Burst, 10)
	}
	h.Set("RateLimit-Policy", policy)
	h.Set("RateLimit-Limit", strconv.FormatInt(res.Limit, 10))
	h.Set("RateLimit-Remaining", strconv.FormatInt(res.Remaining, 10))
	h.Set("RateLimit-Reset", strconv.FormatInt(seconds(res.ResetAfter), 10))
}

type resetRequest struct {
	Rule string `json:"rule"`
	Key  string `json:"key"`
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	var req resetRequest
	if !decode(w, r, &req) {
		return
	}
	rule, ok := s.lookup(w, req.Rule, req.Key)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	if err := s.backend.Reset(ctx, rule, req.Key); err != nil {
		s.metrics.backendErrors.WithLabelValues("reset").Inc()
		s.log.Error("backend reset failed", "rule", rule.Name, "err", err)
		writeError(w, http.StatusServiceUnavailable, "backend unavailable")
		return
	}
	s.log.Info("limit reset", "rule", rule.Name, "key", req.Key)
	w.WriteHeader(http.StatusNoContent)
}

type ruleView struct {
	Name      string `json:"name"`
	Algorithm string `json:"algorithm"`
	Limit     int64  `json:"limit"`
	Window    string `json:"window"`
	WindowMs  int64  `json:"window_ms"`
	Burst     int64  `json:"burst,omitempty"`
	Capacity  int64  `json:"capacity"`
	Spec      string `json:"spec"`
}

func (s *Server) handleRules(w http.ResponseWriter, _ *http.Request) {
	all := s.rules.Load().All()
	out := make([]ruleView, 0, len(all))
	for _, r := range all {
		out = append(out, ruleView{
			Name:      r.Name,
			Algorithm: string(r.Algorithm),
			Limit:     r.Limit,
			Window:    ratelimit.FormatWindow(r.Window),
			WindowMs:  r.Window.Milliseconds(),
			Burst:     r.Burst,
			Capacity:  r.Capacity(),
			Spec:      r.Spec(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": out})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady reports a broken backend as unready only in fail-closed mode.
// When failing open the instance still answers every check, and pulling all
// replicas out of the load balancer at once would turn a Redis outage into a
// full one.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
	defer cancel()
	err := s.backend.Ping(ctx)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case s.failOpen:
		writeJSON(w, http.StatusOK, map[string]string{"status": "degraded", "backend": err.Error()})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "backend": err.Error()})
	}
}

func (s *Server) lookup(w http.ResponseWriter, name, key string) (ratelimit.Rule, bool) {
	if name == "" || key == "" {
		writeError(w, http.StatusBadRequest, "rule and key are required")
		return ratelimit.Rule{}, false
	}
	if len(key) > maxKeyLength {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("key longer than %d bytes", maxKeyLength))
		return ratelimit.Rule{}, false
	}
	rule, ok := s.rules.Load().Get(name)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown rule %q", name))
		return ratelimit.Rule{}, false
	}
	return rule, true
}

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		}
		return false
	}
	if _, err := dec.Token(); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid JSON: trailing data")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func seconds(d time.Duration) int64 {
	return int64(math.Ceil(d.Seconds()))
}

// sampler lets one event through per interval. It keeps a Redis outage from
// turning into one log line per request.
type sampler struct {
	every time.Duration
	last  atomic.Int64
}

func (s *sampler) ok() bool {
	now := time.Now().UnixNano()
	last := s.last.Load()
	return now-last >= s.every.Nanoseconds() && s.last.CompareAndSwap(last, now)
}
