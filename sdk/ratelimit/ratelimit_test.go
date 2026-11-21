package ratelimit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSpecConfigValidates(t *testing.T) {
	cfg, err := (&Spec{MinInterval: "250ms", MaxInterval: "1s", DailyMaxRequests: 10, MaxConcurrency: 1, JitterPercent: 0.2, BackoffBase: "2s", BackoffMax: "1m", BackoffMultiplier: 1.5}).Config()
	if err != nil {
		t.Fatal(err)
	}
	want := Config{MinInterval: 250 * time.Millisecond, MaxInterval: time.Second, DailyMaxRequests: 10, MaxConcurrency: 1, JitterPercent: 0.2, BackoffBase: 2 * time.Second, BackoffMax: time.Minute, BackoffMultiplier: 1.5}
	if cfg != want || !cfg.Active() {
		t.Fatalf("Config() = %+v, want %+v", cfg, want)
	}
	if cfg, err := (*Spec)(nil).Config(); err != nil || cfg.Active() {
		t.Fatalf("nil spec = %+v, %v", cfg, err)
	}
	for name, spec := range map[string]Spec{
		"bad duration":      {MinInterval: "soon"},
		"negative duration": {BackoffBase: "-1s"},
		"negative daily":    {DailyMaxRequests: -1},
		"negative conc":     {MaxConcurrency: -1},
		"jitter above one":  {JitterPercent: 1.5},
		"max below min":     {MinInterval: "2s", MaxInterval: "1s"},
		"negative multiply": {BackoffMultiplier: -2},
	} {
		if _, err := spec.Config(); err == nil {
			t.Errorf("%s: Config() accepted %+v", name, spec)
		}
	}
}

func TestAcquireWithMaxConcurrencyOneQueuesRequests(t *testing.T) {
	limiter := New()
	cfg := Config{MaxConcurrency: 1}
	var inFlight, peak, completed atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			release, err := limiter.Acquire(context.Background(), "credential", cfg)
			if err != nil {
				t.Errorf("Acquire() error = %v, want queued admission", err)
				return
			}
			current := inFlight.Add(1)
			for {
				old := peak.Load()
				if current <= old || peak.CompareAndSwap(old, current) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
			completed.Add(1)
			release()
		})
	}
	wg.Wait()
	if peak.Load() != 1 || completed.Load() != 8 {
		t.Fatalf("peak in flight = %d, completed = %d; want 1 and 8", peak.Load(), completed.Load())
	}
	state, ok := limiter.Snapshot("credential", cfg)
	if !ok || state.InFlight != 0 || state.Waiting != 0 || state.TotalRequests != 8 {
		t.Fatalf("Snapshot() = %+v, %t", state, ok)
	}
}

func TestAcquireReportsQueueAndCancelsWait(t *testing.T) {
	limiter := New()
	cfg := Config{MaxConcurrency: 1}
	release, err := limiter.Acquire(context.Background(), "route", cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, errAcquire := limiter.Acquire(ctx, "route", cfg)
		done <- errAcquire
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if state, _ := limiter.Snapshot("route", cfg); state.Waiting == 1 && state.InFlight == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second request did not queue")
		}
		time.Sleep(time.Millisecond)
	}
	if limiter.Available("route", cfg) {
		t.Fatal("Available() = true while the only slot is in flight")
	}
	cancel()
	if errAcquire := <-done; !errors.Is(errAcquire, context.Canceled) {
		t.Fatalf("queued Acquire() error = %v, want context.Canceled", errAcquire)
	}
	release()
	release()
	if state, _ := limiter.Snapshot("route", cfg); state.Waiting != 0 || state.InFlight != 0 {
		t.Fatalf("state after cancel and release = %+v", state)
	}
}

func TestAcquireEnforcesDailyCap(t *testing.T) {
	limiter := New()
	cfg := Config{DailyMaxRequests: 2}
	for range 2 {
		release, err := limiter.Acquire(context.Background(), "key", cfg)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	_, err := limiter.Acquire(context.Background(), "key", cfg)
	var dailyErr *DailyLimitError
	if !errors.As(err, &dailyErr) {
		t.Fatalf("third Acquire() error = %v, want DailyLimitError", err)
	}
	wantReset := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	if !dailyErr.ResetAt.Equal(wantReset) {
		t.Fatalf("ResetAt = %s, want %s", dailyErr.ResetAt, wantReset)
	}
	if limiter.Available("key", cfg) {
		t.Fatal("Available() = true after the daily cap")
	}
	if _, err := limiter.Acquire(context.Background(), "other-key", cfg); err != nil {
		t.Fatalf("other key shares the daily cap: %v", err)
	}

	limiter.now = func() time.Time { return wantReset.Add(time.Second) }
	release, err := limiter.Acquire(context.Background(), "key", cfg)
	if err != nil {
		t.Fatalf("Acquire() after the daily window rolled over: %v", err)
	}
	release()
	if state, _ := limiter.Snapshot("key", cfg); state.DailyRequests != 1 {
		t.Fatalf("daily requests after rollover = %d, want 1", state.DailyRequests)
	}
}

func TestAcquireSpacesRequestStarts(t *testing.T) {
	limiter := New()
	cfg := Config{MinInterval: 40 * time.Millisecond}
	start := time.Now()
	for range 3 {
		release, err := limiter.Acquire(context.Background(), "key", cfg)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("three starts took %s, want at least two intervals", elapsed)
	}
}

func TestFailureBackoffPausesUntilSuccess(t *testing.T) {
	limiter := New()
	cfg := Config{BackoffBase: time.Minute, BackoffMax: 3 * time.Minute, BackoffMultiplier: 2}
	limiter.MarkFailure("key", cfg)
	state, _ := limiter.Snapshot("key", cfg)
	if state.FailCount != 1 || time.Until(state.CooldownUntil) < 50*time.Second {
		t.Fatalf("after one failure = %+v", state)
	}
	if limiter.Available("key", cfg) {
		t.Fatal("Available() = true during backoff")
	}
	for range 4 {
		limiter.MarkFailure("key", cfg)
	}
	if state, _ := limiter.Snapshot("key", cfg); time.Until(state.CooldownUntil) > 3*time.Minute {
		t.Fatalf("backoff exceeded backoff-max: %+v", state)
	}
	limiter.MarkSuccess("key")
	if state, _ := limiter.Snapshot("key", cfg); state.FailCount != 0 || !state.CooldownUntil.IsZero() || !limiter.Available("key", cfg) {
		t.Fatalf("after success = %+v", state)
	}
}

func TestInactiveConfigNeverWaits(t *testing.T) {
	limiter := New()
	release, err := limiter.Acquire(context.Background(), "key", Config{})
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, ok := limiter.Snapshot("key", Config{}); ok {
		t.Fatal("inactive configuration created limiter state")
	}
}
