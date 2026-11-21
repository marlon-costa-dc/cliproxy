package management

import (
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelrouting"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/ratelimit"
)

// inventoryRateLimit reports a configured limit and its live counters.
func inventoryRateLimit(state ratelimit.State, now time.Time) *modelrouting.InventoryRateLimit {
	report := &modelrouting.InventoryRateLimit{
		MinIntervalMS:    state.Config.MinInterval.Milliseconds(),
		MaxIntervalMS:    state.Config.MaxInterval.Milliseconds(),
		DailyMaxRequests: state.Config.DailyMaxRequests,
		MaxConcurrency:   state.Config.MaxConcurrency,
		InFlight:         state.InFlight,
		Waiting:          state.Waiting,
		DailyRequests:    state.DailyRequests,
		FailCount:        state.FailCount,
	}
	future := func(instant time.Time) *string {
		if !instant.After(now) {
			return nil
		}
		value := instant.UTC().Format(time.RFC3339Nano)
		return &value
	}
	report.DailyResetsAt = future(state.DailyResetAt)
	report.NextAllowedAt = future(state.NextAllowedAt)
	report.CooldownUntil = future(state.CooldownUntil)
	return report
}

// annotateInventoryRateLimits adds the live local rate-limit state of each
// published route and credential that configures one.
func annotateInventoryRateLimits(inventory *modelrouting.Inventory, manager *coreauth.Manager, auths map[string]*coreauth.Auth, now time.Time) {
	if manager == nil {
		return
	}
	byReference := make(map[string]*coreauth.Auth, len(auths))
	for id, auth := range auths {
		byReference[coreauth.CredentialReferenceID(id)] = auth
	}
	for modelIndex := range inventory.DirectModels {
		routes := inventory.DirectModels[modelIndex].Routes
		for routeIndex := range routes {
			route := &routes[routeIndex]
			if state, ok := manager.RouteRateLimitState(route.RouteSelector); ok {
				route.RateLimit = inventoryRateLimit(state, now)
			}
			for credentialIndex := range route.Credentials {
				credential := &route.Credentials[credentialIndex]
				if state, ok := manager.CredentialRateLimitState(byReference[credential.CredentialRef.ID]); ok {
					credential.RateLimit = inventoryRateLimit(state, now)
				}
			}
		}
	}
}
