package kiro

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/ratelimit"
	log "github.com/sirupsen/logrus"
)

const (
	DefaultMinTokenInterval  = 1 * time.Second
	DefaultMaxTokenInterval  = 2 * time.Second
	DefaultDailyMaxRequests  = 500
	DefaultJitterPercent     = 0.3
	DefaultBackoffBase       = 30 * time.Second
	DefaultBackoffMax        = 5 * time.Minute
	DefaultBackoffMultiplier = 1.5
	DefaultSuspendCooldown   = 1 * time.Hour
)

// TokenState tracks per-token request and cooldown state.
type TokenState struct {
	LastRequest    time.Time
	RequestCount   int
	CooldownEnd    time.Time
	FailCount      int
	DailyRequests  int
	DailyResetTime time.Time
	IsSuspended    bool
	SuspendedAt    time.Time
	SuspendReason  string
}

type tokenSuspension struct {
	at     time.Time
	reason string
}

// RateLimiter maps kiro-rate-limit onto the provider-neutral limiter and adds
// Kiro's suspension detection.
type RateLimiter struct {
	mu              sync.RWMutex
	enabled         bool
	cfg             ratelimit.Config
	suspendCooldown time.Duration
	limiter         *ratelimit.Limiter
	suspensions     map[string]tokenSuspension
}

// NewRateLimiter creates a rate limiter with default settings.
func NewRateLimiter() *RateLimiter {
	rl := &RateLimiter{limiter: ratelimit.New(), suspensions: make(map[string]tokenSuspension)}
	rl.applyConfigLocked(RateLimiterConfig{})
	return rl
}

// RateLimiterConfig defines rate limiter settings.
type RateLimiterConfig struct {
	Enabled           *bool
	MinTokenInterval  time.Duration
	MaxTokenInterval  time.Duration
	DailyMaxRequests  int
	JitterPercent     float64
	BackoffBase       time.Duration
	BackoffMax        time.Duration
	BackoffMultiplier float64
	SuspendCooldown   time.Duration
}

// NewRateLimiterWithConfig creates a rate limiter with custom settings.
func NewRateLimiterWithConfig(cfg RateLimiterConfig) *RateLimiter {
	rl := NewRateLimiter()
	rl.applyConfigLocked(cfg)
	return rl
}

// ApplyConfig reapplies config defaults and then applies overrides.
func (rl *RateLimiter) ApplyConfig(cfg RateLimiterConfig) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.applyConfigLocked(cfg)
}

func (rl *RateLimiter) applyConfigLocked(cfg RateLimiterConfig) {
	rl.enabled = cfg.Enabled != nil && *cfg.Enabled
	rl.cfg = ratelimit.Config{
		MinInterval:       positiveOr(cfg.MinTokenInterval, DefaultMinTokenInterval),
		MaxInterval:       positiveOr(cfg.MaxTokenInterval, DefaultMaxTokenInterval),
		DailyMaxRequests:  DefaultDailyMaxRequests,
		JitterPercent:     DefaultJitterPercent,
		BackoffBase:       positiveOr(cfg.BackoffBase, DefaultBackoffBase),
		BackoffMax:        positiveOr(cfg.BackoffMax, DefaultBackoffMax),
		BackoffMultiplier: DefaultBackoffMultiplier,
	}
	if cfg.DailyMaxRequests > 0 {
		rl.cfg.DailyMaxRequests = cfg.DailyMaxRequests
	}
	if cfg.JitterPercent > 0 {
		rl.cfg.JitterPercent = cfg.JitterPercent
	}
	if cfg.BackoffMultiplier > 0 {
		rl.cfg.BackoffMultiplier = cfg.BackoffMultiplier
	}
	rl.suspendCooldown = positiveOr(cfg.SuspendCooldown, DefaultSuspendCooldown)
	if rl.cfg.MaxInterval <= rl.cfg.MinInterval {
		log.Warnf("kiro: rate limiter max-token-interval (%v) <= min-token-interval (%v), clamping max to min+1s",
			rl.cfg.MaxInterval, rl.cfg.MinInterval)
		rl.cfg.MaxInterval = rl.cfg.MinInterval + time.Second
	}
}

func positiveOr(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func (rl *RateLimiter) settings() (bool, ratelimit.Config, time.Duration) {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	return rl.enabled, rl.cfg, rl.suspendCooldown
}

// WaitForToken blocks until the token is allowed to send another request. The
// daily cap is reported by IsTokenAvailable and never blocks the request.
func (rl *RateLimiter) WaitForToken(tokenKey string) {
	enabled, cfg, _ := rl.settings()
	if !enabled {
		return
	}
	pacing := cfg
	pacing.DailyMaxRequests = 0
	release, errAcquire := rl.limiter.Acquire(context.Background(), tokenKey, pacing)
	if errAcquire == nil {
		release()
	}
}

// MarkTokenFailed records a failed request for a token.
func (rl *RateLimiter) MarkTokenFailed(tokenKey string) {
	if enabled, cfg, _ := rl.settings(); enabled {
		rl.limiter.MarkFailure(tokenKey, cfg)
	}
}

// MarkTokenSuccess clears any failure state after a successful request.
func (rl *RateLimiter) MarkTokenSuccess(tokenKey string) {
	if enabled, _, _ := rl.settings(); enabled {
		rl.limiter.MarkSuccess(tokenKey)
	}
}

var suspendKeywords = []string{
	"suspended",
	"banned",
	"disabled",
	"account has been",
	"access denied",
	"rate limit exceeded",
	"too many requests",
	"quota exceeded",
}

// CheckAndMarkSuspended detects suspension-like errors and records them.
func (rl *RateLimiter) CheckAndMarkSuspended(tokenKey string, errorMsg string) bool {
	enabled, _, suspendCooldown := rl.settings()
	if !enabled {
		return false
	}
	lowerMsg := strings.ToLower(errorMsg)
	for _, keyword := range suspendKeywords {
		if strings.Contains(lowerMsg, keyword) {
			now := time.Now()
			rl.mu.Lock()
			rl.suspensions[tokenKey] = tokenSuspension{at: now, reason: errorMsg}
			rl.mu.Unlock()
			rl.limiter.CoolDown(tokenKey, now.Add(suspendCooldown))
			return true
		}
	}
	return false
}

// IsTokenAvailable reports whether the token can currently be used.
func (rl *RateLimiter) IsTokenAvailable(tokenKey string) bool {
	enabled, cfg, suspendCooldown := rl.settings()
	if !enabled {
		return true
	}
	rl.mu.RLock()
	suspension, suspended := rl.suspensions[tokenKey]
	rl.mu.RUnlock()
	if suspended {
		// Suspended tokens stay unavailable until their suspension window expires.
		return time.Now().After(suspension.at.Add(suspendCooldown))
	}
	return rl.limiter.Available(tokenKey, cfg)
}

// GetTokenState returns a copy of the token state, or nil for an unknown token.
func (rl *RateLimiter) GetTokenState(tokenKey string) *TokenState {
	_, cfg, _ := rl.settings()
	state, known := rl.limiter.Snapshot(tokenKey, cfg)
	rl.mu.RLock()
	suspension, suspended := rl.suspensions[tokenKey]
	rl.mu.RUnlock()
	if !known && !suspended {
		return nil
	}
	return &TokenState{
		LastRequest:    state.LastRequestAt,
		RequestCount:   int(state.TotalRequests),
		CooldownEnd:    state.CooldownUntil,
		FailCount:      state.FailCount,
		DailyRequests:  state.DailyRequests,
		DailyResetTime: state.DailyResetAt,
		IsSuspended:    suspended,
		SuspendedAt:    suspension.at,
		SuspendReason:  suspension.reason,
	}
}

// ClearTokenState removes all tracked state for a token.
func (rl *RateLimiter) ClearTokenState(tokenKey string) {
	rl.mu.Lock()
	delete(rl.suspensions, tokenKey)
	rl.mu.Unlock()
	rl.limiter.Forget(tokenKey)
}

// ResetSuspension clears a token's suspension and cooldown markers.
func (rl *RateLimiter) ResetSuspension(tokenKey string) {
	rl.mu.Lock()
	delete(rl.suspensions, tokenKey)
	rl.mu.Unlock()
	rl.limiter.MarkSuccess(tokenKey)
}
