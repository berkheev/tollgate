package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Result struct {
	Allowed   bool
	Limit     int64
	Remaining int64
	// ResetAfter is the time until the full quota is available again.
	ResetAfter time.Duration
	// RetryAfter is zero when the request was allowed.
	RetryAfter time.Duration
}

type Backend interface {
	Check(ctx context.Context, rule Rule, key string, cost int64) (Result, error)
	Reset(ctx context.Context, rule Rule, key string) error
	Ping(ctx context.Context) error
	Close() error
}

var ErrInvalidCost = errors.New("invalid cost")

func validateCost(r Rule, cost int64) error {
	if cost < 1 {
		return fmt.Errorf("%w: must be at least 1", ErrInvalidCost)
	}
	if c := r.Capacity(); cost > c {
		return fmt.Errorf("%w: %d exceeds capacity %d of rule %s", ErrInvalidCost, cost, c, r.Name)
	}
	return nil
}

func storageKey(prefix string, r Rule, key string) string {
	return prefix + ":" + r.Name + ":" + algorithms[r.Algorithm] + ":" + key
}
