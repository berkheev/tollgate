package ratelimit

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/berkheev/tollgate/internal/clock"
)

// Integration tests run when TOLLGATE_TEST_REDIS_URL points at a Redis
// server, e.g. redis://localhost:6379/15. Every test uses its own key prefix.
func redisClient(tb testing.TB) *redis.Client {
	tb.Helper()
	url := os.Getenv("TOLLGATE_TEST_REDIS_URL")
	if url == "" {
		tb.Skip("TOLLGATE_TEST_REDIS_URL not set")
	}
	opts, err := redis.ParseURL(url)
	if err != nil {
		tb.Fatal(err)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(context.Background()).Err(); err != nil {
		tb.Fatalf("redis not reachable: %v", err)
	}
	tb.Cleanup(func() { _ = client.Close() })
	return client
}

func newTestRedis(tb testing.TB, client *redis.Client, c clock.Clock) *Redis {
	tb.Helper()
	prefix := fmt.Sprintf("tollgate-test-%x", rand.Uint64())
	tb.Cleanup(func() {
		ctx := context.Background()
		iter := client.Scan(ctx, 0, prefix+":*", 500).Iterator()
		for iter.Next(ctx) {
			client.Del(ctx, iter.Val())
		}
	})
	return NewRedis(client, c, prefix)
}

func TestRedis(t *testing.T) {
	client := redisClient(t)
	runConformance(t, func(t *testing.T, c clock.Clock) Backend {
		return newTestRedis(t, client, c)
	})
}

func TestRedisRecoversFromScriptFlush(t *testing.T) {
	client := redisClient(t)
	b := newTestRedis(t, client, clock.NewFake(epoch))
	r := mustRule(t, "flush", "sliding_window 10/1m")
	ctx := context.Background()

	if err := b.LoadScripts(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.ScriptFlush(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	res, err := b.Check(ctx, r, "k", 1)
	if err != nil {
		t.Fatalf("check after SCRIPT FLUSH: %v", err)
	}
	if !res.Allowed || res.Remaining != 9 {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestRedisKeysExpire(t *testing.T) {
	client := redisClient(t)
	b := newTestRedis(t, client, clock.Real{})
	ctx := context.Background()

	for _, spec := range []string{"token_bucket 10/1m", "fixed_window 10/1m", "sliding_window 10/1m", "sliding_log 10/1m"} {
		r := mustRule(t, "ttl", spec)
		if _, err := b.Check(ctx, r, "k", 1); err != nil {
			t.Fatal(err)
		}
		ttl, err := client.PTTL(ctx, storageKey(b.prefix, r, "k")).Result()
		if err != nil {
			t.Fatal(err)
		}
		if ttl <= 0 || ttl > 2*r.Window {
			t.Fatalf("%s: ttl %s out of range", spec, ttl)
		}
	}
}

func BenchmarkRedis(b *testing.B) {
	client := redisClient(b)
	benchmarkBackend(b, newTestRedis(b, client, clock.Real{}))
}
