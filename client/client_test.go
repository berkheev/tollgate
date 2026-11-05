package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/berkheev/tollgate/client"
	"github.com/berkheev/tollgate/internal/clock"
	"github.com/berkheev/tollgate/internal/ratelimit"
	"github.com/berkheev/tollgate/internal/server"
)

func newTollgate(t *testing.T) (*client.Client, *clock.Fake) {
	t.Helper()
	c := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	api, _ := ratelimit.ParseRule("api", "token_bucket 1/s burst 2")
	login, _ := ratelimit.ParseRule("login", "fixed_window 1/1m")
	rules, err := ratelimit.NewRuleSet(api, login)
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(server.Options{Backend: ratelimit.NewMemory(c), Rules: rules})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return client.New(ts.URL), c
}

func TestCheck(t *testing.T) {
	tg, c := newTollgate(t)
	ctx := context.Background()

	d, err := tg.CheckN(ctx, "api", "u1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed || d.Remaining != 0 || d.Limit != 2 || d.ResetAfter != 2*time.Second || d.Policy != "1;w=1;burst=2" {
		t.Fatalf("unexpected decision %+v", d)
	}

	d, err = tg.Check(ctx, "api", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed || d.RetryAfter != time.Second {
		t.Fatalf("expected denial with 1s retry, got %+v", d)
	}

	c.Advance(time.Second)
	if d, _ := tg.Check(ctx, "api", "u1"); !d.Allowed {
		t.Fatalf("expected refill, got %+v", d)
	}
}

func TestErrors(t *testing.T) {
	tg, _ := newTollgate(t)
	_, err := tg.Check(context.Background(), "missing", "k")
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 APIError, got %v", err)
	}

	_, err = tg.CheckN(context.Background(), "api", "k", 50)
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 APIError, got %v", err)
	}

	down := client.New("http://127.0.0.1:1", client.WithTimeout(200*time.Millisecond))
	if _, err := down.Check(context.Background(), "api", "k"); err == nil {
		t.Fatal("expected transport error")
	}
}

func TestResetAndRules(t *testing.T) {
	tg, _ := newTollgate(t)
	ctx := context.Background()

	if _, err := tg.Check(ctx, "login", "ip"); err != nil {
		t.Fatal(err)
	}
	if d, _ := tg.Check(ctx, "login", "ip"); d.Allowed {
		t.Fatal("expected denial")
	}
	if err := tg.Reset(ctx, "login", "ip"); err != nil {
		t.Fatal(err)
	}
	if d, _ := tg.Check(ctx, "login", "ip"); !d.Allowed {
		t.Fatal("expected allow after reset")
	}

	rules, err := tg.Rules(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 || rules[0].Name != "api" || rules[0].Capacity != 2 || rules[1].Spec != "fixed_window 1/1m" {
		t.Fatalf("unexpected rules %+v", rules)
	}
}

func TestMiddleware(t *testing.T) {
	tg, _ := newTollgate(t)
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := client.Middleware(tg, "api", client.ByHeader("X-User"))(ok)

	call := func(user string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/orders", nil)
		if user != "" {
			req.Header.Set("X-User", user)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for i, want := range []int{http.StatusNoContent, http.StatusNoContent, http.StatusTooManyRequests} {
		rec := call("alice")
		if rec.Code != want {
			t.Fatalf("request %d: status %d, want %d", i, rec.Code, want)
		}
		if rec.Header().Get("RateLimit-Limit") != "2" {
			t.Fatalf("request %d: missing RateLimit-Limit", i)
		}
	}
	if rec := call("alice"); rec.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q", rec.Header().Get("Retry-After"))
	}
	if rec := call("bob"); rec.Code != http.StatusNoContent {
		t.Fatalf("bob should have his own bucket, got %d", rec.Code)
	}
	if rec := call(""); rec.Code != http.StatusNoContent || rec.Header().Get("RateLimit-Limit") != "" {
		t.Fatal("requests without a key must bypass the limiter")
	}
}

func TestMiddlewareWhenTollgateIsDown(t *testing.T) {
	down := client.New("http://127.0.0.1:1", client.WithTimeout(200*time.Millisecond))
	ok := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })

	var errs int
	open := client.Middleware(down, "api", client.ByRemoteIP(), client.OnError(func(*http.Request, error) { errs++ }))(ok)
	closed := client.Middleware(down, "api", client.ByRemoteIP(), client.FailClosed())(ok)

	rec := httptest.NewRecorder()
	open.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusNoContent || errs != 1 {
		t.Fatalf("fail-open: status %d, errors %d", rec.Code, errs)
	}

	rec = httptest.NewRecorder()
	closed.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("fail-closed: status %d", rec.Code)
	}
}
