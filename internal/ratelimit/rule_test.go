package ratelimit

import (
	"testing"
	"time"
)

func TestParseRule(t *testing.T) {
	tests := []struct {
		spec    string
		want    Rule
		wantErr bool
	}{
		{spec: "token_bucket 100/min burst 20", want: Rule{Algorithm: TokenBucket, Limit: 100, Window: time.Minute, Burst: 20}},
		{spec: "sliding_window 5/15m", want: Rule{Algorithm: SlidingWindow, Limit: 5, Window: 15 * time.Minute}},
		{spec: "fixed_window 1000/1d", want: Rule{Algorithm: FixedWindow, Limit: 1000, Window: 24 * time.Hour}},
		{spec: "sliding_log 10/second", want: Rule{Algorithm: SlidingLog, Limit: 10, Window: time.Second}},
		{spec: "fixed_window 50/1h30m", want: Rule{Algorithm: FixedWindow, Limit: 50, Window: 90 * time.Minute}},
		{spec: "token_bucket   20/500ms", want: Rule{Algorithm: TokenBucket, Limit: 20, Window: 500 * time.Millisecond}},
		{spec: "leaky_bucket 10/s", wantErr: true},
		{spec: "fixed_window 10", wantErr: true},
		{spec: "fixed_window 0/s", wantErr: true},
		{spec: "fixed_window ten/s", wantErr: true},
		{spec: "fixed_window 10/fortnight", wantErr: true},
		{spec: "fixed_window 10/-1s", wantErr: true},
		{spec: "fixed_window 10/1500us", wantErr: true},
		{spec: "fixed_window 10/s burst 5", wantErr: true},
		{spec: "token_bucket 10/s burst", wantErr: true},
		{spec: "token_bucket 10/s capacity 5", wantErr: true},
		{spec: "sliding_log 20000/h", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			got, err := ParseRule("api", tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.want.Name = "api"
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestRuleSpecRoundTrip(t *testing.T) {
	for _, spec := range []string{
		"token_bucket 100/1m burst 20",
		"sliding_window 5/15m",
		"fixed_window 1000/1d",
		"sliding_log 3/10s",
		"token_bucket 20/500ms",
	} {
		r, err := ParseRule("x", spec)
		if err != nil {
			t.Fatal(err)
		}
		if r.Spec() != spec {
			t.Fatalf("Spec() = %q, want %q", r.Spec(), spec)
		}
	}
}

func TestRuleCapacity(t *testing.T) {
	if c := mustRule(t, "a", "token_bucket 100/min burst 20").Capacity(); c != 20 {
		t.Fatalf("capacity %d, want 20", c)
	}
	if c := mustRule(t, "a", "token_bucket 100/min").Capacity(); c != 100 {
		t.Fatalf("capacity %d, want 100", c)
	}
	if c := mustRule(t, "a", "fixed_window 7/min").Capacity(); c != 7 {
		t.Fatalf("capacity %d, want 7", c)
	}
}

func TestNewRuleSet(t *testing.T) {
	a := mustRule(t, "b_rule", "fixed_window 1/s")
	b := mustRule(t, "a_rule", "fixed_window 1/s")
	rs, err := NewRuleSet(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if all := rs.All(); all[0].Name != "a_rule" || all[1].Name != "b_rule" {
		t.Fatalf("rules not sorted: %v", all)
	}
	if _, ok := rs.Get("a_rule"); !ok {
		t.Fatal("a_rule missing")
	}

	if _, err := NewRuleSet(a, a); err == nil {
		t.Fatal("expected duplicate error")
	}
	if _, err := NewRuleSet(Rule{Name: "bad name!", Algorithm: FixedWindow, Limit: 1, Window: time.Second}); err == nil {
		t.Fatal("expected name validation error")
	}
}
