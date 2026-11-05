package ratelimit

import (
	"math"
	"sort"
	"time"
)

// The algorithms below work on integer milliseconds and are mirrored
// one-to-one by the Lua scripts in lua/. Keep both in sync: the conformance
// tests run the same cases against the memory and the Redis backend.

type outcome struct {
	allowed   bool
	remaining int64
	resetMs   int64
	retryMs   int64
	ttlMs     int64
	dirty     bool
}

func (o outcome) result(r Rule) Result {
	return Result{
		Allowed:    o.allowed,
		Limit:      r.Capacity(),
		Remaining:  max(o.remaining, 0),
		ResetAfter: time.Duration(max(o.resetMs, 0)) * time.Millisecond,
		RetryAfter: time.Duration(max(o.retryMs, 0)) * time.Millisecond,
	}
}

type state interface {
	take(r Rule, now, cost int64) outcome
}

func newState(r Rule, now int64) state {
	switch r.Algorithm {
	case TokenBucket:
		return &tokenBucket{tokens: float64(r.Capacity()), updated: now}
	case FixedWindow:
		return &fixedWindow{}
	case SlidingWindow:
		return &slidingWindow{}
	case SlidingLog:
		return &slidingLog{}
	}
	panic("ratelimit: unknown algorithm " + string(r.Algorithm))
}

type tokenBucket struct {
	tokens  float64
	updated int64
}

func (s *tokenBucket) take(r Rule, now, cost int64) outcome {
	capacity := float64(r.Capacity())
	limit := float64(r.Limit)
	window := float64(r.Window.Milliseconds())

	tokens := s.tokens
	if elapsed := now - s.updated; elapsed > 0 {
		tokens = math.Min(capacity, tokens+float64(elapsed)*limit/window)
	}

	var o outcome
	if tokens >= float64(cost) {
		tokens -= float64(cost)
		s.tokens, s.updated = tokens, max(now, s.updated)
		o.allowed, o.dirty = true, true
	} else {
		o.retryMs = ceil((float64(cost) - tokens) * window / limit)
	}
	o.remaining = int64(math.Floor(tokens))
	o.resetMs = ceil((capacity - tokens) * window / limit)
	o.ttlMs = max(o.resetMs, 1)
	return o
}

type fixedWindow struct {
	start int64
	count int64
}

func (s *fixedWindow) take(r Rule, now, cost int64) outcome {
	w := r.Window.Milliseconds()
	if start := now - now%w; start > s.start {
		s.start, s.count = start, 0
	}

	var o outcome
	o.resetMs = s.start + w - now
	if s.count+cost <= r.Limit {
		s.count += cost
		o.allowed, o.dirty = true, true
	} else {
		o.retryMs = o.resetMs
	}
	o.remaining = r.Limit - s.count
	o.ttlMs = o.resetMs
	return o
}

// slidingWindow approximates a rolling window from two fixed windows: the
// previous window's count is weighted by how much of it still overlaps the
// rolling window. All math is scaled by the window length to stay integral.
type slidingWindow struct {
	start int64
	curr  int64
	prev  int64
}

func (s *slidingWindow) take(r Rule, now, cost int64) outcome {
	w := r.Window.Milliseconds()
	limit := r.Limit

	switch start := now - now%w; {
	case start <= s.start:
	case start == s.start+w:
		s.start, s.prev, s.curr = start, s.curr, 0
	default:
		s.start, s.prev, s.curr = start, 0, 0
	}

	elapsed := min(max(now-s.start, 0), w)
	used := s.prev*(w-elapsed) + s.curr*w

	var o outcome
	if used+cost*w <= limit*w {
		s.curr += cost
		used += cost * w
		o.allowed, o.dirty = true, true
	} else if spare := limit - s.curr - cost; spare >= 0 {
		// The previous window has to fade until prev*(w-e) <= spare*w.
		o.retryMs = s.start + ceilDiv(w*(s.prev-spare), s.prev) - now
	} else {
		// Not even a fully faded previous window helps: wait for the next
		// one, where the current count becomes the weighted part.
		o.retryMs = s.start + w + ceilDiv(w*(s.curr-(limit-cost)), s.curr) - now
	}

	o.remaining = (limit*w - used) / w
	switch {
	case s.curr > 0:
		o.resetMs = s.start + 2*w - now
	case s.prev > 0:
		o.resetMs = s.start + w - now
	}
	o.ttlMs = s.start + 2*w - now
	return o
}

// slidingLog keeps one timestamp per consumed unit, sorted ascending.
type slidingLog struct {
	entries []int64
}

func (s *slidingLog) take(r Rule, now, cost int64) outcome {
	w := r.Window.Milliseconds()
	cut := sort.Search(len(s.entries), func(i int) bool { return s.entries[i] > now-w })
	if cut > 0 {
		s.entries = append(s.entries[:0], s.entries[cut:]...)
	}

	var o outcome
	count := int64(len(s.entries))
	if count+cost <= r.Limit {
		at := sort.Search(len(s.entries), func(i int) bool { return s.entries[i] > now })
		add := make([]int64, cost)
		for i := range add {
			add[i] = now
		}
		s.entries = append(s.entries[:at], append(add, s.entries[at:]...)...)
		o.allowed, o.dirty = true, true
	} else {
		o.retryMs = s.entries[count+cost-r.Limit-1] + w - now
	}

	o.remaining = r.Limit - int64(len(s.entries))
	if n := len(s.entries); n > 0 {
		o.resetMs = s.entries[n-1] + w - now
	}
	o.ttlMs = max(o.resetMs, 1)
	return o
}

func ceil(f float64) int64 {
	return int64(math.Ceil(f))
}

func ceilDiv(a, b int64) int64 {
	return (a + b - 1) / b
}
