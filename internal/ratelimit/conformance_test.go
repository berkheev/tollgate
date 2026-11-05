package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/berkheev/tollgate/internal/clock"
)

// epoch is aligned to whole days so fixed windows start at offset zero.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type newBackendFunc func(t *testing.T, c clock.Clock) Backend

type step struct {
	advance   time.Duration
	cost      int64
	allowed   bool
	remaining int64
	reset     time.Duration
	retry     time.Duration
}

type algorithmCase struct {
	name  string
	rule  string
	steps []step
}

var algorithmCases = []algorithmCase{
	{
		name: "token bucket with burst",
		rule: "token_bucket 10/s burst 5",
		steps: []step{
			{cost: 1, allowed: true, remaining: 4, reset: 100 * time.Millisecond},
			{cost: 4, allowed: true, remaining: 0, reset: 500 * time.Millisecond},
			{cost: 1, allowed: false, remaining: 0, reset: 500 * time.Millisecond, retry: 100 * time.Millisecond},
			{advance: 50 * time.Millisecond, cost: 1, allowed: false, remaining: 0, reset: 450 * time.Millisecond, retry: 50 * time.Millisecond},
			{advance: 50 * time.Millisecond, cost: 1, allowed: true, remaining: 0, reset: 500 * time.Millisecond},
			{advance: time.Second, cost: 3, allowed: true, remaining: 2, reset: 300 * time.Millisecond},
			{cost: 3, allowed: false, remaining: 2, reset: 300 * time.Millisecond, retry: 100 * time.Millisecond},
		},
	},
	{
		name: "token bucket without burst refills one token per interval",
		rule: "token_bucket 3/min",
		steps: []step{
			{cost: 3, allowed: true, remaining: 0, reset: time.Minute},
			{cost: 1, allowed: false, remaining: 0, reset: time.Minute, retry: 20 * time.Second},
			{advance: 20 * time.Second, cost: 1, allowed: true, remaining: 0, reset: time.Minute},
			{advance: 10 * time.Minute, cost: 1, allowed: true, remaining: 2, reset: 20 * time.Second},
		},
	},
	{
		name: "fixed window",
		rule: "fixed_window 3/1m",
		steps: []step{
			{advance: 10 * time.Second, cost: 1, allowed: true, remaining: 2, reset: 50 * time.Second},
			{cost: 2, allowed: true, remaining: 0, reset: 50 * time.Second},
			{cost: 1, allowed: false, remaining: 0, reset: 50 * time.Second, retry: 50 * time.Second},
			{advance: 49*time.Second + 999*time.Millisecond, cost: 1, allowed: false, remaining: 0, reset: time.Millisecond, retry: time.Millisecond},
			{advance: time.Millisecond, cost: 1, allowed: true, remaining: 2, reset: time.Minute},
		},
	},
	{
		name: "sliding window weights the previous window",
		rule: "sliding_window 10/10s",
		steps: []step{
			{cost: 5, allowed: true, remaining: 5, reset: 20 * time.Second},
			{advance: 10 * time.Second, cost: 5, allowed: true, remaining: 0, reset: 20 * time.Second},
			{cost: 1, allowed: false, remaining: 0, reset: 20 * time.Second, retry: 2 * time.Second},
			{advance: 2 * time.Second, cost: 1, allowed: true, remaining: 0, reset: 18 * time.Second},
			{cost: 5, allowed: false, remaining: 0, reset: 18 * time.Second, retry: 9667 * time.Millisecond},
			{advance: 9666 * time.Millisecond, cost: 5, allowed: false, remaining: 4, reset: 8334 * time.Millisecond, retry: time.Millisecond},
			{advance: time.Millisecond, cost: 5, allowed: true, remaining: 0, reset: 18333 * time.Millisecond},
			{advance: 30 * time.Second, cost: 1, allowed: true, remaining: 9, reset: 18333 * time.Millisecond},
		},
	},
	{
		name: "sliding log",
		rule: "sliding_log 3/1m",
		steps: []step{
			{cost: 1, allowed: true, remaining: 2, reset: time.Minute},
			{advance: 10 * time.Second, cost: 1, allowed: true, remaining: 1, reset: time.Minute},
			{advance: 10 * time.Second, cost: 1, allowed: true, remaining: 0, reset: time.Minute},
			{advance: 5 * time.Second, cost: 1, allowed: false, remaining: 0, reset: 55 * time.Second, retry: 35 * time.Second},
			{advance: 35 * time.Second, cost: 2, allowed: false, remaining: 1, reset: 20 * time.Second, retry: 10 * time.Second},
			{advance: 10 * time.Second, cost: 2, allowed: true, remaining: 0, reset: time.Minute},
		},
	},
}

func runConformance(t *testing.T, newBackend newBackendFunc) {
	t.Run("algorithms", func(t *testing.T) {
		for _, tc := range algorithmCases {
			t.Run(tc.name, func(t *testing.T) {
				c := clock.NewFake(epoch)
				b := newBackend(t, c)
				r := mustRule(t, "case", tc.rule)

				for i, s := range tc.steps {
					c.Advance(s.advance)
					got, err := b.Check(context.Background(), r, "user-1", s.cost)
					if err != nil {
						t.Fatalf("step %d: %v", i, err)
					}
					want := Result{
						Allowed:    s.allowed,
						Limit:      r.Capacity(),
						Remaining:  s.remaining,
						ResetAfter: s.reset,
						RetryAfter: s.retry,
					}
					if got != want {
						t.Fatalf("step %d (t=%s):\n got %+v\nwant %+v", i, c.Now().Sub(epoch), got, want)
					}
				}
			})
		}
	})

	t.Run("keys are independent", func(t *testing.T) {
		b := newBackend(t, clock.NewFake(epoch))
		r := mustRule(t, "indep", "fixed_window 1/1m")
		for _, key := range []string{"a", "b"} {
			res, err := b.Check(context.Background(), r, key, 1)
			if err != nil || !res.Allowed {
				t.Fatalf("first request for %s: %+v, %v", key, res, err)
			}
		}
		res, _ := b.Check(context.Background(), r, "a", 1)
		if res.Allowed {
			t.Fatal("second request for a should be denied")
		}
	})

	t.Run("rules with the same key are independent", func(t *testing.T) {
		b := newBackend(t, clock.NewFake(epoch))
		r1 := mustRule(t, "one", "fixed_window 1/1m")
		r2 := mustRule(t, "two", "fixed_window 1/1m")
		for _, r := range []Rule{r1, r2} {
			if res, _ := b.Check(context.Background(), r, "k", 1); !res.Allowed {
				t.Fatalf("rule %s should allow", r.Name)
			}
		}
	})

	t.Run("reset clears state", func(t *testing.T) {
		for _, spec := range []string{"token_bucket 2/h", "fixed_window 2/h", "sliding_window 2/h", "sliding_log 2/h"} {
			b := newBackend(t, clock.NewFake(epoch))
			r := mustRule(t, "reset", spec)
			ctx := context.Background()
			if _, err := b.Check(ctx, r, "k", 2); err != nil {
				t.Fatal(err)
			}
			if res, _ := b.Check(ctx, r, "k", 1); res.Allowed {
				t.Fatalf("%s: expected denial before reset", spec)
			}
			if err := b.Reset(ctx, r, "k"); err != nil {
				t.Fatal(err)
			}
			if res, _ := b.Check(ctx, r, "k", 2); !res.Allowed {
				t.Fatalf("%s: expected full quota after reset, got %+v", spec, res)
			}
		}
	})

	t.Run("rejects invalid cost", func(t *testing.T) {
		b := newBackend(t, clock.NewFake(epoch))
		r := mustRule(t, "cost", "token_bucket 100/s burst 10")
		for _, cost := range []int64{0, -1, 11} {
			if _, err := b.Check(context.Background(), r, "k", cost); !errors.Is(err, ErrInvalidCost) {
				t.Fatalf("cost %d: want ErrInvalidCost, got %v", cost, err)
			}
		}
	})

	t.Run("concurrent checks never over-admit", func(t *testing.T) {
		for _, spec := range []string{"token_bucket 100/h", "fixed_window 100/h", "sliding_window 100/h", "sliding_log 100/h"} {
			b := newBackend(t, clock.NewFake(epoch))
			r := mustRule(t, "race", spec)

			var allowed atomic.Int64
			var wg sync.WaitGroup
			for range 25 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for range 10 {
						res, err := b.Check(context.Background(), r, "shared", 1)
						if err != nil {
							t.Error(err)
							return
						}
						if res.Allowed {
							allowed.Add(1)
						}
					}
				}()
			}
			wg.Wait()
			if got := allowed.Load(); got != 100 {
				t.Fatalf("%s: allowed %d of 250, want exactly 100", spec, got)
			}
		}
	})
}

func mustRule(t testing.TB, name, spec string) Rule {
	t.Helper()
	r, err := ParseRule(name, spec)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func BenchmarkMemory(b *testing.B) {
	benchmarkBackend(b, NewMemory(clock.Real{}))
}

func benchmarkBackend(b *testing.B, backend Backend) {
	specs := []string{
		"token_bucket 1000000/s",
		"fixed_window 1000000/s",
		"sliding_window 1000000/s",
		"sliding_log 1000/s",
	}
	for _, spec := range specs {
		r, err := ParseRule("bench", spec)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(string(r.Algorithm), func(b *testing.B) {
			keys := make([]string, 1024)
			for i := range keys {
				keys[i] = fmt.Sprintf("user-%d", i)
			}
			var n atomic.Uint64
			b.ReportAllocs()
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					key := keys[n.Add(1)%uint64(len(keys))]
					if _, err := backend.Check(context.Background(), r, key, 1); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}
