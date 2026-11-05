package ratelimit

import (
	"context"
	"hash/maphash"
	"sync"
	"time"

	"github.com/berkheev/tollgate/internal/clock"
)

const shardCount = 64

// Memory keeps limiter state in process. It is exact for a single instance
// and is what the tests run against; use Redis when several instances share
// limits.
type Memory struct {
	clock  clock.Clock
	seed   maphash.Seed
	shards [shardCount]memoryShard
}

type memoryShard struct {
	mu    sync.Mutex
	items map[string]*memoryItem
}

type memoryItem struct {
	state     state
	expiresAt int64
}

func NewMemory(c clock.Clock) *Memory {
	m := &Memory{clock: c, seed: maphash.MakeSeed()}
	for i := range m.shards {
		m.shards[i].items = make(map[string]*memoryItem)
	}
	return m
}

func (m *Memory) shard(key string) *memoryShard {
	return &m.shards[maphash.String(m.seed, key)%shardCount]
}

func (m *Memory) Check(_ context.Context, r Rule, key string, cost int64) (Result, error) {
	if err := validateCost(r, cost); err != nil {
		return Result{}, err
	}
	now := m.clock.Now().UnixMilli()
	k := storageKey("", r, key)
	s := m.shard(k)

	s.mu.Lock()
	defer s.mu.Unlock()

	it, ok := s.items[k]
	if !ok || it.expiresAt <= now {
		it = &memoryItem{state: newState(r, now)}
		ok = false
	}
	o := it.state.take(r, now, cost)
	if o.dirty {
		it.expiresAt = now + o.ttlMs
		if !ok {
			s.items[k] = it
		}
	}
	return o.result(r), nil
}

func (m *Memory) Reset(_ context.Context, r Rule, key string) error {
	k := storageKey("", r, key)
	s := m.shard(k)
	s.mu.Lock()
	delete(s.items, k)
	s.mu.Unlock()
	return nil
}

func (m *Memory) Ping(context.Context) error { return nil }

func (m *Memory) Close() error { return nil }

// Sweep drops expired entries. Expired state is equivalent to no state, so
// this only bounds memory use.
func (m *Memory) Sweep() int {
	now := m.clock.Now().UnixMilli()
	removed := 0
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.Lock()
		for k, it := range s.items {
			if it.expiresAt <= now {
				delete(s.items, k)
				removed++
			}
		}
		s.mu.Unlock()
	}
	return removed
}

func (m *Memory) Len() int {
	n := 0
	for i := range m.shards {
		s := &m.shards[i]
		s.mu.Lock()
		n += len(s.items)
		s.mu.Unlock()
	}
	return n
}

func (m *Memory) RunSweeper(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Sweep()
		}
	}
}
