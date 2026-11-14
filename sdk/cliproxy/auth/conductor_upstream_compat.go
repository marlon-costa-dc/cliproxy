package auth

import (
	"context"
	"time"
)

// newUpstreamAttemptContext creates the connected-attempt context for Home paths.
func (m *Manager) newUpstreamAttemptContext(ctx context.Context) context.Context {
	return newUpstreamAttemptContext(ctx)
}

func isForceCooldownError(err *Error) bool {
	return err != nil && err.Code == ErrorCodeForceCooldown
}

func effectiveAuthForceUntil(auth *Auth) time.Time {
	if auth == nil {
		return time.Time{}
	}
	if auth.forceCooldownSeeded {
		return auth.forceCooldownUntil
	}
	if len(auth.ModelStates) == 0 && isForceCooldownError(auth.LastError) {
		return auth.NextRetryAfter
	}
	return time.Time{}
}

// ModelAvailability reports why selection skips a credential for a model.
type ModelAvailability struct {
	Blocked       bool
	QuotaCooldown bool
	RecoverAt     time.Time
}

// AvailabilityForModel returns the selection availability of auth for model at now.
func AvailabilityForModel(auth *Auth, model string, now time.Time) ModelAvailability {
	blocked, reason, next := isAuthBlockedForModel(auth, model, now)
	return ModelAvailability{Blocked: blocked, QuotaCooldown: blocked && reason == blockReasonCooldown, RecoverAt: next}
}
