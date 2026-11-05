package ratelimit

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Algorithm string

const (
	TokenBucket   Algorithm = "token_bucket"
	FixedWindow   Algorithm = "fixed_window"
	SlidingWindow Algorithm = "sliding_window"
	SlidingLog    Algorithm = "sliding_log"
)

var algorithms = map[Algorithm]string{
	TokenBucket:   "tb",
	FixedWindow:   "fw",
	SlidingWindow: "sw",
	SlidingLog:    "sl",
}

func (a Algorithm) Valid() bool {
	_, ok := algorithms[a]
	return ok
}

// Rule is a named limit: Limit units per Window. For token_bucket the window
// defines the refill rate and Burst (defaults to Limit) the bucket size.
type Rule struct {
	Name      string
	Algorithm Algorithm
	Limit     int64
	Window    time.Duration
	Burst     int64
}

// MaxSlidingLogLimit keeps the per-key sorted set small. Use sliding_window
// for anything bigger.
const MaxSlidingLogLimit = 10_000

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,64}$`)

func (r Rule) Validate() error {
	if !nameRe.MatchString(r.Name) {
		return fmt.Errorf("rule name %q: only letters, digits, '_', '.', '-' allowed (max 64)", r.Name)
	}
	if !r.Algorithm.Valid() {
		return fmt.Errorf("rule %s: unknown algorithm %q", r.Name, r.Algorithm)
	}
	if r.Limit <= 0 {
		return fmt.Errorf("rule %s: limit must be positive", r.Name)
	}
	if r.Window < time.Millisecond || r.Window%time.Millisecond != 0 {
		return fmt.Errorf("rule %s: window must be a whole number of milliseconds", r.Name)
	}
	if r.Burst < 0 {
		return fmt.Errorf("rule %s: burst must not be negative", r.Name)
	}
	if r.Burst > 0 && r.Algorithm != TokenBucket {
		return fmt.Errorf("rule %s: burst is only supported by token_bucket", r.Name)
	}
	if r.Algorithm == SlidingLog && r.Limit > MaxSlidingLogLimit {
		return fmt.Errorf("rule %s: sliding_log limit is capped at %d", r.Name, MaxSlidingLogLimit)
	}
	return nil
}

// Capacity is the largest cost a single request can have.
func (r Rule) Capacity() int64 {
	if r.Algorithm == TokenBucket && r.Burst > 0 {
		return r.Burst
	}
	return r.Limit
}

func (r Rule) Spec() string {
	s := fmt.Sprintf("%s %d/%s", r.Algorithm, r.Limit, FormatWindow(r.Window))
	if r.Burst > 0 {
		s += fmt.Sprintf(" burst %d", r.Burst)
	}
	return s
}

// ParseRule parses the short form used in the config file:
//
//	token_bucket 100/min burst 20
//	sliding_window 5/15m
func ParseRule(name, spec string) (Rule, error) {
	fields := strings.Fields(spec)
	if len(fields) != 2 && len(fields) != 4 {
		return Rule{}, fmt.Errorf("rule %s: expected \"<algorithm> <limit>/<window> [burst <n>]\", got %q", name, spec)
	}
	r := Rule{Name: name, Algorithm: Algorithm(fields[0])}

	limit, window, err := ParseRate(fields[1])
	if err != nil {
		return Rule{}, fmt.Errorf("rule %s: %w", name, err)
	}
	r.Limit, r.Window = limit, window

	if len(fields) == 4 {
		if fields[2] != "burst" {
			return Rule{}, fmt.Errorf("rule %s: unexpected %q", name, fields[2])
		}
		r.Burst, err = strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return Rule{}, fmt.Errorf("rule %s: invalid burst %q", name, fields[3])
		}
	}
	return r, r.Validate()
}

var unitWords = map[string]time.Duration{
	"ms": time.Millisecond,
	"s":  time.Second, "sec": time.Second, "second": time.Second,
	"m": time.Minute, "min": time.Minute, "minute": time.Minute,
	"h": time.Hour, "hour": time.Hour,
	"d": 24 * time.Hour, "day": 24 * time.Hour,
}

// ParseRate parses "100/min", "5/15m", "10/s", "1000/1d".
func ParseRate(s string) (int64, time.Duration, error) {
	count, per, ok := strings.Cut(s, "/")
	if !ok {
		return 0, 0, fmt.Errorf("invalid rate %q", s)
	}
	limit, err := strconv.ParseInt(count, 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("invalid limit in %q", s)
	}
	window, err := ParseWindow(per)
	if err != nil {
		return 0, 0, err
	}
	return limit, window, nil
}

func ParseWindow(s string) (time.Duration, error) {
	if d, ok := unitWords[s]; ok {
		return d, nil
	}
	i := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' })
	if i > 0 {
		if unit, ok := unitWords[s[i:]]; ok {
			n, err := strconv.ParseInt(s[:i], 10, 64)
			if err == nil && n > 0 {
				return time.Duration(n) * unit, nil
			}
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid window %q", s)
	}
	return d, nil
}

func FormatWindow(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return d.String()
}

// RuleSet is an immutable collection of rules keyed by name.
type RuleSet struct {
	byName map[string]Rule
	sorted []Rule
}

func NewRuleSet(rules ...Rule) (*RuleSet, error) {
	rs := &RuleSet{byName: make(map[string]Rule, len(rules))}
	var errs []error
	for _, r := range rules {
		if err := r.Validate(); err != nil {
			errs = append(errs, err)
			continue
		}
		if _, dup := rs.byName[r.Name]; dup {
			errs = append(errs, fmt.Errorf("duplicate rule %s", r.Name))
			continue
		}
		rs.byName[r.Name] = r
		rs.sorted = append(rs.sorted, r)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	sort.Slice(rs.sorted, func(i, j int) bool { return rs.sorted[i].Name < rs.sorted[j].Name })
	return rs, nil
}

func (rs *RuleSet) Get(name string) (Rule, bool) {
	r, ok := rs.byName[name]
	return r, ok
}

func (rs *RuleSet) All() []Rule {
	return rs.sorted
}

func (rs *RuleSet) Len() int {
	return len(rs.sorted)
}
