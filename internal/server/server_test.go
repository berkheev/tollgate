package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/berkheev/tollgate/internal/clock"
	"github.com/berkheev/tollgate/internal/ratelimit"
)

var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func testRules(t *testing.T) *ratelimit.RuleSet {
	t.Helper()
	var rules []ratelimit.Rule
	for name, spec := range map[string]string{
		"api":   "token_bucket 10/s burst 2",
		"login": "sliding_log 3/15m",
	} {
		r, err := ratelimit.ParseRule(name, spec)
		if err != nil {
			t.Fatal(err)
		}
		rules = append(rules, r)
	}
	rs, err := ratelimit.NewRuleSet(rules...)
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

type fixture struct {
	srv   *Server
	http  *httptest.Server
	clock *clock.Fake
}

func newFixture(t *testing.T, backend ratelimit.Backend, failOpen bool) *fixture {
	t.Helper()
	c := clock.NewFake(epoch)
	if backend == nil {
		backend = ratelimit.NewMemory(c)
	}
	reg := prometheus.NewRegistry()
	srv := New(Options{
		Backend:    backend,
		Rules:      testRules(t),
		FailOpen:   failOpen,
		Registerer: reg,
		Gatherer:   reg,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &fixture{srv: srv, http: ts, clock: c}
}

func (f *fixture) post(t *testing.T, path, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(f.http.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode %q: %v", raw, err)
		}
	}
	return resp, out
}

func TestCheckAllowedThenDenied(t *testing.T) {
	f := newFixture(t, nil, true)

	resp, body := f.post(t, "/v1/check", `{"rule":"api","key":"u1","cost":2}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if body["allowed"] != true || body["remaining"] != 0.0 || body["limit"] != 2.0 || body["reset_after_ms"] != 200.0 {
		t.Fatalf("unexpected body %v", body)
	}
	wantHeaders := map[string]string{
		"RateLimit-Limit":     "2",
		"RateLimit-Remaining": "0",
		"RateLimit-Reset":     "1",
		"RateLimit-Policy":    "10;w=1;burst=2",
	}
	for k, v := range wantHeaders {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	resp, body = f.post(t, "/v1/check", `{"rule":"api","key":"u1"}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if body["allowed"] != false || body["retry_after_ms"] != 100.0 {
		t.Fatalf("unexpected body %v", body)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q", got)
	}

	f.clock.Advance(100 * time.Millisecond)
	if resp, _ := f.post(t, "/v1/check", `{"rule":"api","key":"u1"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("after refill: status %d", resp.StatusCode)
	}

	allowed := testutil.ToFloat64(f.srv.metrics.decisions.WithLabelValues("api", "allowed"))
	denied := testutil.ToFloat64(f.srv.metrics.decisions.WithLabelValues("api", "denied"))
	if allowed != 2 || denied != 1 {
		t.Fatalf("metrics allowed=%v denied=%v", allowed, denied)
	}
}

func TestCheckValidation(t *testing.T) {
	f := newFixture(t, nil, true)
	tests := []struct {
		body   string
		status int
	}{
		{`{"rule":"nope","key":"k"}`, http.StatusNotFound},
		{`{"rule":"api"}`, http.StatusBadRequest},
		{`{"key":"k"}`, http.StatusBadRequest},
		{`{"rule":"api","key":"k","cost":0}`, http.StatusUnprocessableEntity},
		{`{"rule":"api","key":"k","cost":3}`, http.StatusUnprocessableEntity},
		{`{"rule":"api","key":"k","cots":1}`, http.StatusBadRequest},
		{`{"rule":"api","key":"k"} {}`, http.StatusBadRequest},
		{`not json`, http.StatusBadRequest},
		{`{"rule":"api","key":"` + strings.Repeat("x", 600) + `"}`, http.StatusBadRequest},
		{`{"rule":"api","key":"` + strings.Repeat("x", 20000) + `"}`, http.StatusRequestEntityTooLarge},
	}
	for _, tt := range tests {
		resp, body := f.post(t, "/v1/check", tt.body)
		if resp.StatusCode != tt.status {
			t.Errorf("%.60s: status %d, want %d (%v)", tt.body, resp.StatusCode, tt.status, body)
		}
		if body["error"] == nil {
			t.Errorf("%.60s: missing error message", tt.body)
		}
	}
}

func TestReset(t *testing.T) {
	f := newFixture(t, nil, true)
	for range 3 {
		f.post(t, "/v1/check", `{"rule":"login","key":"10.0.0.1"}`)
	}
	if resp, _ := f.post(t, "/v1/check", `{"rule":"login","key":"10.0.0.1"}`); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}
	if resp, _ := f.post(t, "/v1/reset", `{"rule":"login","key":"10.0.0.1"}`); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("reset status %d", resp.StatusCode)
	}
	if resp, _ := f.post(t, "/v1/check", `{"rule":"login","key":"10.0.0.1"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after reset, got %d", resp.StatusCode)
	}
}

func TestRules(t *testing.T) {
	f := newFixture(t, nil, true)
	resp, err := http.Get(f.http.URL + "/v1/rules")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Rules []ruleView `json:"rules"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Rules) != 2 || out.Rules[0].Name != "api" || out.Rules[1].Name != "login" {
		t.Fatalf("unexpected rules %+v", out.Rules)
	}
	api := out.Rules[0]
	if api.Spec != "token_bucket 10/1s burst 2" || api.Capacity != 2 || api.WindowMs != 1000 {
		t.Fatalf("unexpected api rule %+v", api)
	}
}

func TestSetRules(t *testing.T) {
	f := newFixture(t, nil, true)
	r, _ := ratelimit.ParseRule("new_rule", "fixed_window 1/s")
	rs, _ := ratelimit.NewRuleSet(r)
	f.srv.SetRules(rs)

	if resp, _ := f.post(t, "/v1/check", `{"rule":"new_rule","key":"k"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("new rule: status %d", resp.StatusCode)
	}
	if resp, _ := f.post(t, "/v1/check", `{"rule":"api","key":"k"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("removed rule: status %d", resp.StatusCode)
	}
}

type brokenBackend struct{}

var errDown = errors.New("connection refused")

func (brokenBackend) Check(context.Context, ratelimit.Rule, string, int64) (ratelimit.Result, error) {
	return ratelimit.Result{}, errDown
}
func (brokenBackend) Reset(context.Context, ratelimit.Rule, string) error { return errDown }
func (brokenBackend) Ping(context.Context) error                          { return errDown }
func (brokenBackend) Close() error                                        { return nil }

func TestFailureModes(t *testing.T) {
	tests := []struct {
		failOpen    bool
		checkStatus int
		allowed     bool
		readyStatus int
	}{
		{failOpen: true, checkStatus: http.StatusOK, allowed: true, readyStatus: http.StatusOK},
		{failOpen: false, checkStatus: http.StatusTooManyRequests, allowed: false, readyStatus: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		f := newFixture(t, &brokenBackend{}, tt.failOpen)

		resp, body := f.post(t, "/v1/check", `{"rule":"api","key":"k"}`)
		if resp.StatusCode != tt.checkStatus || body["allowed"] != tt.allowed || body["degraded"] != true {
			t.Fatalf("fail_open=%v: status %d body %v", tt.failOpen, resp.StatusCode, body)
		}
		if resp.Header.Get("Tollgate-Degraded") != "true" {
			t.Fatalf("fail_open=%v: missing degraded header", tt.failOpen)
		}
		if resp.Header.Get("RateLimit-Remaining") != "" {
			t.Fatalf("fail_open=%v: rate limit headers must not be guessed", tt.failOpen)
		}
		if got := testutil.ToFloat64(f.srv.metrics.backendErrors.WithLabelValues("check")); got != 1 {
			t.Fatalf("backend errors = %v", got)
		}

		ready, err := http.Get(f.http.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		ready.Body.Close()
		if ready.StatusCode != tt.readyStatus {
			t.Fatalf("fail_open=%v: readyz %d", tt.failOpen, ready.StatusCode)
		}

		if resp, _ := f.post(t, "/v1/reset", `{"rule":"api","key":"k"}`); resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("reset with broken backend: %d", resp.StatusCode)
		}
	}
}

func TestOperationalEndpoints(t *testing.T) {
	f := newFixture(t, nil, true)
	f.post(t, "/v1/check", `{"rule":"api","key":"k"}`)

	for path, want := range map[string]int{
		"/healthz":    http.StatusOK,
		"/readyz":     http.StatusOK,
		"/metrics":    http.StatusOK,
		"/nope":       http.StatusNotFound,
		"/v1/check":   http.StatusMethodNotAllowed,
		"/v1/rules/x": http.StatusNotFound,
	} {
		resp, err := http.Get(f.http.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s: %d, want %d", path, resp.StatusCode, want)
		}
		if path == "/metrics" {
			for _, name := range []string{"tollgate_decisions_total", "tollgate_check_duration_seconds", "tollgate_http_requests_total", "tollgate_rules 2"} {
				if !strings.Contains(string(body), name) {
					t.Errorf("metrics output lacks %s", name)
				}
			}
		}
	}

	unmatched := testutil.ToFloat64(f.srv.metrics.httpRequests.WithLabelValues("unmatched", "404"))
	if unmatched != 2 {
		t.Fatalf("unmatched requests = %v, want 2", unmatched)
	}
}

func TestSampler(t *testing.T) {
	s := sampler{every: time.Hour}
	if !s.ok() {
		t.Fatal("first event must pass")
	}
	if s.ok() {
		t.Fatal("second event within the interval must be dropped")
	}
}
