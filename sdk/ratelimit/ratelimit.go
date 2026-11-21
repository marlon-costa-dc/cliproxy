// Package ratelimit paces requests per key: one credential, one published
// route, or any other provider-neutral scope. A key may bound its request
// spacing, daily volume and concurrency, and backs off after failures.
package ratelimit

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"
)

// Spec is the authored rate-limit block shared by API-key entries and
// published routes. Durations use Go syntax ("1s", "250ms"). Omitted fields
// apply no limit.
type Spec struct {
	// MinInterval is the minimum spacing between two request starts.
	MinInterval string `yaml:"min-interval,omitempty" json:"min-interval,omitempty"`
	// MaxInterval randomizes the spacing up to this value (default MinInterval).
	MaxInterval string `yaml:"max-interval,omitempty" json:"max-interval,omitempty"`
	// DailyMaxRequests caps request starts per UTC day.
	DailyMaxRequests int `yaml:"daily-max-requests,omitempty" json:"daily-max-requests,omitempty"`
	// MaxConcurrency bounds requests in flight; further requests queue.
	MaxConcurrency int `yaml:"max-concurrency,omitempty" json:"max-concurrency,omitempty"`
	// JitterPercent varies each spacing by up to this fraction (0 to 1).
	JitterPercent float64 `yaml:"jitter-percent,omitempty" json:"jitter-percent,omitempty"`
	// BackoffBase is the pause after the first consecutive failure.
	BackoffBase string `yaml:"backoff-base,omitempty" json:"backoff-base,omitempty"`
	// BackoffMax caps the failure pause.
	BackoffMax string `yaml:"backoff-max,omitempty" json:"backoff-max,omitempty"`
	// BackoffMultiplier grows the pause per consecutive failure (default 2).
	BackoffMultiplier float64 `yaml:"backoff-multiplier,omitempty" json:"backoff-multiplier,omitempty"`
}

// Config is a validated Spec.
type Config struct {
	MinInterval       time.Duration `json:"min_interval,omitempty"`
	MaxInterval       time.Duration `json:"max_interval,omitempty"`
	DailyMaxRequests  int           `json:"daily_max_requests,omitempty"`
	MaxConcurrency    int           `json:"max_concurrency,omitempty"`
	JitterPercent     float64       `json:"jitter_percent,omitempty"`
	BackoffBase       time.Duration `json:"backoff_base,omitempty"`
	BackoffMax        time.Duration `json:"backoff_max,omitempty"`
	BackoffMultiplier float64       `json:"backoff_multiplier,omitempty"`
}

const defaultBackoffMultiplier = 2

// Config validates the block. A nil Spec yields the zero Config.
func (s *Spec) Config() (Config, error) {
	if s == nil {
		return Config{}, nil
	}
	cfg := Config{
		DailyMaxRequests:  s.DailyMaxRequests,
		MaxConcurrency:    s.MaxConcurrency,
		JitterPercent:     s.JitterPercent,
		BackoffMultiplier: s.BackoffMultiplier,
	}
	for _, field := range []struct {
		name  string
		value string
		dest  *time.Duration
	}{
		{"min-interval", s.MinInterval, &cfg.MinInterval},
		{"max-interval", s.MaxInterval, &cfg.MaxInterval},
		{"backoff-base", s.BackoffBase, &cfg.BackoffBase},
		{"backoff-max", s.BackoffMax, &cfg.BackoffMax},
	} {
		value := strings.TrimSpace(field.value)
		if value == "" {
			continue
		}
		parsed, errParse := time.ParseDuration(value)
		if errParse != nil || parsed < 0 {
			return Config{}, fmt.Errorf("%s: must be a non-negative duration", field.name)
		}
		*field.dest = parsed
	}
	switch {
	case cfg.DailyMaxRequests < 0:
		return Config{}, fmt.Errorf("daily-max-requests: must not be negative")
	case cfg.MaxConcurrency < 0:
		return Config{}, fmt.Errorf("max-concurrency: must not be negative")
	case cfg.JitterPercent < 0 || cfg.JitterPercent > 1:
		return Config{}, fmt.Errorf("jitter-percent: must be between 0 and 1")
	case cfg.BackoffMultiplier < 0:
		return Config{}, fmt.Errorf("backoff-multiplier: must not be negative")
	case cfg.MaxInterval > 0 && cfg.MaxInterval < cfg.MinInterval:
		return Config{}, fmt.Errorf("max-interval: must not be below min-interval")
	}
	return cfg, nil
}

// Active reports whether the configuration limits anything.
func (c Config) Active() bool {
	return c.MinInterval > 0 || c.MaxInterval > 0 || c.DailyMaxRequests > 0 || c.MaxConcurrency > 0 || c.BackoffBase > 0
}

// DailyLimitError reports that a key exhausted its daily request cap.
type DailyLimitError struct {
	ResetAt time.Time
}

func (e *DailyLimitError) Error() string {
	return "daily request limit reached until " + e.ResetAt.UTC().Format(time.RFC3339)
}

// State is a key's current pacing state.
type State struct {
	Config        Config    `json:"config"`
	InFlight      int       `json:"in_flight"`
	Waiting       int       `json:"waiting"`
	TotalRequests int64     `json:"total_requests"`
	DailyRequests int       `json:"daily_requests"`
	DailyResetAt  time.Time `json:"daily_reset_at"`
	LastRequestAt time.Time `json:"last_request_at"`
	NextAllowedAt time.Time `json:"next_allowed_at"`
	CooldownUntil time.Time `json:"cooldown_until"`
	FailCount     int       `json:"fail_count"`
}

type keyState struct {
	inFlight    int
	waiting     int
	total       int64
	daily       int
	dailyReset  time.Time
	lastStart   time.Time
	nextAllowed time.Time
	cooldownEnd time.Time
	failCount   int
	released    chan struct{}
}

// Limiter holds per-key pacing state. The zero value is not usable; use New.
type Limiter struct {
	mu   sync.Mutex
	keys map[string]*keyState
	rng  *rand.Rand
	now  func() time.Time
}

// New returns an empty limiter.
func New() *Limiter {
	return &Limiter{
		keys: make(map[string]*keyState),
		rng:  rand.New(rand.NewSource(time.Now().UnixNano())),
		now:  time.Now,
	}
}

func (l *Limiter) stateLocked(key string, now time.Time) *keyState {
	state, ok := l.keys[key]
	if !ok {
		state = &keyState{released: make(chan struct{})}
		l.keys[key] = state
	}
	if !now.Before(state.dailyReset) {
		state.daily = 0
		state.dailyReset = now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	}
	return state
}

// Acquire blocks until cfg admits one more request for key and returns the
// function that ends it. Requests beyond MaxConcurrency queue instead of
// failing; a reached daily cap returns *DailyLimitError; ctx ends the wait.
func (l *Limiter) Acquire(ctx context.Context, key string, cfg Config) (func(), error) {
	if !cfg.Active() {
		return func() {}, nil
	}
	l.mu.Lock()
	state := l.stateLocked(key, l.now())
	state.waiting++
	for {
		now := l.now()
		state = l.stateLocked(key, now)
		if cfg.DailyMaxRequests > 0 && state.daily >= cfg.DailyMaxRequests {
			state.waiting--
			resetAt := state.dailyReset
			l.mu.Unlock()
			return nil, &DailyLimitError{ResetAt: resetAt}
		}
		var released chan struct{}
		var wait time.Duration
		if cfg.MaxConcurrency > 0 && state.inFlight >= cfg.MaxConcurrency {
			released = state.released
		} else if next := laterTime(state.nextAllowed, state.cooldownEnd); now.Before(next) {
			wait = next.Sub(now)
		} else {
			state.waiting--
			state.inFlight++
			state.total++
			state.daily++
			state.lastStart = now
			state.nextAllowed = now.Add(l.intervalLocked(cfg))
			l.mu.Unlock()
			var once sync.Once
			return func() { once.Do(func() { l.release(key) }) }, nil
		}
		l.mu.Unlock()
		if errWait := waitFor(ctx, released, wait); errWait != nil {
			l.mu.Lock()
			state.waiting--
			l.mu.Unlock()
			return nil, errWait
		}
		l.mu.Lock()
	}
}

func waitFor(ctx context.Context, released <-chan struct{}, wait time.Duration) error {
	if released != nil {
		select {
		case <-released:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Limiter) release(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	state, ok := l.keys[key]
	if !ok || state.inFlight == 0 {
		return
	}
	state.inFlight--
	close(state.released)
	state.released = make(chan struct{})
}

// MarkFailure counts a consecutive failure and, when cfg has a backoff,
// pauses the key for the exponential backoff.
func (l *Limiter) MarkFailure(key string, cfg Config) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	state := l.stateLocked(key, now)
	state.failCount++
	if backoff := l.backoffLocked(cfg, state.failCount); backoff > 0 {
		state.cooldownEnd = laterTime(state.cooldownEnd, now.Add(backoff))
	}
}

// MarkSuccess clears the failure count and the failure pause.
func (l *Limiter) MarkSuccess(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if state, ok := l.keys[key]; ok {
		state.failCount = 0
		state.cooldownEnd = time.Time{}
	}
}

// CoolDown pauses the key until the given instant.
func (l *Limiter) CoolDown(key string, until time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	state := l.stateLocked(key, l.now())
	state.cooldownEnd = laterTime(state.cooldownEnd, until)
}

// Forget drops all state for the key.
func (l *Limiter) Forget(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.keys, key)
}

// Snapshot returns the key's state under cfg; false when the key was never used.
func (l *Limiter) Snapshot(key string, cfg Config) (State, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.keys[key]; !ok {
		return State{Config: cfg}, false
	}
	state := l.stateLocked(key, l.now())
	return State{
		Config:        cfg,
		InFlight:      state.inFlight,
		Waiting:       state.waiting,
		TotalRequests: state.total,
		DailyRequests: state.daily,
		DailyResetAt:  state.dailyReset,
		LastRequestAt: state.lastStart,
		NextAllowedAt: laterTime(state.nextAllowed, state.cooldownEnd),
		CooldownUntil: state.cooldownEnd,
		FailCount:     state.failCount,
	}, true
}

// Available reports whether the key could start a request now without waiting.
func (l *Limiter) Available(key string, cfg Config) bool {
	if !cfg.Active() {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	state := l.stateLocked(key, now)
	if cfg.DailyMaxRequests > 0 && state.daily >= cfg.DailyMaxRequests {
		return false
	}
	if cfg.MaxConcurrency > 0 && state.inFlight >= cfg.MaxConcurrency {
		return false
	}
	return !now.Before(state.cooldownEnd)
}

// intervalLocked draws the spacing to the next request start.
func (l *Limiter) intervalLocked(cfg Config) time.Duration {
	if cfg.MinInterval <= 0 && cfg.MaxInterval <= 0 {
		return 0
	}
	base := cfg.MinInterval
	if cfg.MaxInterval > cfg.MinInterval {
		base += time.Duration(l.rng.Int63n(int64(cfg.MaxInterval - cfg.MinInterval)))
	}
	interval := base + time.Duration(float64(base)*cfg.JitterPercent*(l.rng.Float64()*2-1))
	if interval < 0 {
		return 0
	}
	return interval
}

// backoffLocked returns the pause after failCount consecutive failures.
func (l *Limiter) backoffLocked(cfg Config, failCount int) time.Duration {
	if cfg.BackoffBase <= 0 || failCount <= 0 {
		return 0
	}
	multiplier := cfg.BackoffMultiplier
	if multiplier <= 0 {
		multiplier = defaultBackoffMultiplier
	}
	backoff := float64(cfg.BackoffBase) * math.Pow(multiplier, float64(failCount-1))
	backoff += backoff * cfg.JitterPercent * (l.rng.Float64()*2 - 1)
	if cfg.BackoffMax > 0 && backoff > float64(cfg.BackoffMax) {
		return cfg.BackoffMax
	}
	return time.Duration(backoff)
}

func laterTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
