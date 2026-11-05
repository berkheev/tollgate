package ratelimit

import (
	"context"
	"testing"
	"time"

	"github.com/berkheev/tollgate/internal/clock"
)

func TestMemory(t *testing.T) {
	runConformance(t, func(t *testing.T, c clock.Clock) Backend {
		return NewMemory(c)
	})
}

func TestMemorySweep(t *testing.T) {
	c := clock.NewFake(epoch)
	m := NewMemory(c)
	short := mustRule(t, "short", "fixed_window 5/1s")
	long := mustRule(t, "long", "fixed_window 5/1h")

	for _, key := range []string{"a", "b", "c"} {
		if _, err := m.Check(context.Background(), short, key, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Check(context.Background(), long, "a", 1); err != nil {
		t.Fatal(err)
	}

	c.Advance(2 * time.Second)
	if removed := m.Sweep(); removed != 3 {
		t.Fatalf("removed %d entries, want 3", removed)
	}
	if n := m.Len(); n != 1 {
		t.Fatalf("%d entries left, want 1", n)
	}
}

func TestMemoryDeniedRequestDoesNotCreateState(t *testing.T) {
	m := NewMemory(clock.NewFake(epoch))
	r := mustRule(t, "x", "fixed_window 1/1m")
	if _, err := m.Check(context.Background(), r, "k", 2); err == nil {
		t.Fatal("expected cost error")
	}
	if m.Len() != 0 {
		t.Fatal("state created for a rejected request")
	}
}

func TestTokenBucketClockGoingBackwards(t *testing.T) {
	c := clock.NewFake(epoch)
	m := NewMemory(c)
	r := mustRule(t, "tb", "token_bucket 1/s burst 2")

	if res, _ := m.Check(context.Background(), r, "k", 2); !res.Allowed {
		t.Fatal("expected first request to pass")
	}
	c.Advance(-5 * time.Second)
	if res, _ := m.Check(context.Background(), r, "k", 1); res.Allowed {
		t.Fatal("a clock step backwards must not refill the bucket")
	}
}
