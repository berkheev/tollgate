package client

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"time"
)

// KeyFunc extracts the rate limit key from a request. An empty key skips
// limiting for that request.
type KeyFunc func(*http.Request) string

func ByHeader(name string) KeyFunc {
	return func(r *http.Request) string { return r.Header.Get(name) }
}

// ByRemoteIP uses the connection address. Behind a proxy, use a KeyFunc that
// reads the header your proxy sets instead.
func ByRemoteIP() KeyFunc {
	return func(r *http.Request) string {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			return r.RemoteAddr
		}
		return host
	}
}

type middlewareConfig struct {
	cost       func(*http.Request) int64
	failClosed bool
	onError    func(*http.Request, error)
	denied     http.Handler
}

type MiddlewareOption func(*middlewareConfig)

func WithCost(f func(*http.Request) int64) MiddlewareOption {
	return func(c *middlewareConfig) { c.cost = f }
}

// FailClosed rejects requests with 503 when tollgate itself cannot be
// reached. The default is to let them through.
func FailClosed() MiddlewareOption {
	return func(c *middlewareConfig) { c.failClosed = true }
}

func OnError(f func(*http.Request, error)) MiddlewareOption {
	return func(c *middlewareConfig) { c.onError = f }
}

// OnDenied replaces the default 429 response. Rate limit headers are already
// set when it runs.
func OnDenied(h http.Handler) MiddlewareOption {
	return func(c *middlewareConfig) { c.denied = h }
}

// Middleware limits requests with the given rule, keyed by key.
func Middleware(c *Client, rule string, key KeyFunc, opts ...MiddlewareOption) func(http.Handler) http.Handler {
	cfg := middlewareConfig{
		denied: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		}),
	}
	for _, o := range opts {
		o(&cfg)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			k := key(r)
			if k == "" {
				next.ServeHTTP(w, r)
				return
			}
			cost := int64(1)
			if cfg.cost != nil {
				cost = cfg.cost(r)
			}

			d, err := c.CheckN(r.Context(), rule, k, cost)
			if err != nil {
				if cfg.onError != nil {
					cfg.onError(r, err)
				}
				if cfg.failClosed {
					http.Error(w, "rate limiter unavailable", http.StatusServiceUnavailable)
					return
				}
				next.ServeHTTP(w, r)
				return
			}

			if !d.Degraded {
				h := w.Header()
				if d.Policy != "" {
					h.Set("RateLimit-Policy", d.Policy)
				}
				h.Set("RateLimit-Limit", strconv.FormatInt(d.Limit, 10))
				h.Set("RateLimit-Remaining", strconv.FormatInt(d.Remaining, 10))
				h.Set("RateLimit-Reset", strconv.FormatInt(ceilSeconds(d.ResetAfter), 10))
			}
			if !d.Allowed {
				w.Header().Set("Retry-After", strconv.FormatInt(max(ceilSeconds(d.RetryAfter), 1), 10))
				cfg.denied.ServeHTTP(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func ceilSeconds(d time.Duration) int64 {
	return int64(math.Ceil(d.Seconds()))
}
