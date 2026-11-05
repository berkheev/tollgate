package ratelimit

import (
	"context"
	_ "embed"
	"fmt"
	"math/rand/v2"
	"strconv"

	"github.com/redis/go-redis/v9"

	"github.com/berkheev/tollgate/internal/clock"
)

var (
	//go:embed lua/token_bucket.lua
	tokenBucketLua string
	//go:embed lua/fixed_window.lua
	fixedWindowLua string
	//go:embed lua/sliding_window.lua
	slidingWindowLua string
	//go:embed lua/sliding_log.lua
	slidingLogLua string

	scripts = map[Algorithm]*redis.Script{
		TokenBucket:   redis.NewScript(tokenBucketLua),
		FixedWindow:   redis.NewScript(fixedWindowLua),
		SlidingWindow: redis.NewScript(slidingWindowLua),
		SlidingLog:    redis.NewScript(slidingLogLua),
	}
)

// Redis evaluates every check as a single Lua script against one key, so a
// decision is atomic no matter how many tollgate instances share the server.
// Time comes from the injected clock rather than Redis TIME, which keeps the
// scripts testable with a fake clock; instances are expected to run NTP.
type Redis struct {
	client redis.UniversalClient
	clock  clock.Clock
	prefix string
}

func NewRedis(client redis.UniversalClient, c clock.Clock, prefix string) *Redis {
	if prefix == "" {
		prefix = "tollgate"
	}
	return &Redis{client: client, clock: c, prefix: prefix}
}

// LoadScripts pushes the scripts into the server cache up front. Not
// required: Run falls back to EVAL when EVALSHA reports NOSCRIPT.
func (b *Redis) LoadScripts(ctx context.Context) error {
	for alg, s := range scripts {
		if err := s.Load(ctx, b.client).Err(); err != nil {
			return fmt.Errorf("load %s script: %w", alg, err)
		}
	}
	return nil
}

func (b *Redis) Check(ctx context.Context, r Rule, key string, cost int64) (Result, error) {
	if err := validateCost(r, cost); err != nil {
		return Result{}, err
	}
	now := b.clock.Now().UnixMilli()
	window := r.Window.Milliseconds()

	var args []any
	switch r.Algorithm {
	case TokenBucket:
		args = []any{now, r.Capacity(), r.Limit, window, cost}
	case FixedWindow, SlidingWindow:
		args = []any{now, r.Limit, window, cost}
	case SlidingLog:
		nonce := strconv.FormatInt(now, 36) + "-" + strconv.FormatUint(rand.Uint64(), 36)
		args = []any{now, r.Limit, window, cost, nonce}
	default:
		return Result{}, fmt.Errorf("unknown algorithm %q", r.Algorithm)
	}

	vals, err := scripts[r.Algorithm].Run(ctx, b.client, []string{storageKey(b.prefix, r, key)}, args...).Int64Slice()
	if err != nil {
		return Result{}, fmt.Errorf("redis %s: %w", r.Algorithm, err)
	}
	if len(vals) != 4 {
		return Result{}, fmt.Errorf("redis %s: unexpected reply %v", r.Algorithm, vals)
	}
	return outcome{
		allowed:   vals[0] == 1,
		remaining: vals[1],
		resetMs:   vals[2],
		retryMs:   vals[3],
	}.result(r), nil
}

func (b *Redis) Reset(ctx context.Context, r Rule, key string) error {
	return b.client.Del(ctx, storageKey(b.prefix, r, key)).Err()
}

func (b *Redis) Ping(ctx context.Context) error {
	return b.client.Ping(ctx).Err()
}

func (b *Redis) Close() error {
	return b.client.Close()
}
