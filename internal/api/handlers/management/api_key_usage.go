package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelrouting"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

type apiKeyUsageEntry struct {
	Success        int64                          `json:"success"`
	Failed         int64                          `json:"failed"`
	RecentRequests []coreauth.RecentRequestBucket `json:"recent_requests"`
	// State is the v8 runtime state of the key: cooldown or suspension with its
	// resume instant, quota facts and quota domain, without provider error text.
	State *modelrouting.InventoryCredential `json:"state,omitempty"`
}

// apiKeyUsageState reports a key with the routing inventory's credential
// decision, evaluated for the whole credential rather than one model.
func apiKeyUsageState(manager *coreauth.Manager, auth *coreauth.Auth, now time.Time) *modelrouting.InventoryCredential {
	route := registry.RegisteredRouteSnapshot{ClientID: auth.ID}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		route.SuspensionReason = "disabled"
	}
	state := routingInventoryCredential(route, auth, auth.RoutingQuotaDomain(), now)
	if limit, ok := manager.CredentialRateLimitState(auth); ok {
		state.RateLimit = inventoryRateLimit(limit, now)
	}
	return &state
}

func mergeRecentRequestBuckets(dst, src []coreauth.RecentRequestBucket) []coreauth.RecentRequestBucket {
	if len(dst) == 0 {
		return src
	}
	if len(src) == 0 {
		return dst
	}
	if len(dst) != len(src) {
		n := len(dst)
		if len(src) < n {
			n = len(src)
		}
		for i := 0; i < n; i++ {
			dst[i].Success += src[i].Success
			dst[i].Failed += src[i].Failed
		}
		return dst
	}
	for i := range dst {
		dst[i].Success += src[i].Success
		dst[i].Failed += src[i].Failed
	}
	return dst
}

func apiKeyUsageProviderKey(auth *coreauth.Auth) string {
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if auth.Attributes != nil {
		if compatName := strings.TrimSpace(auth.Attributes["compat_name"]); compatName != "" {
			provider = strings.ToLower(compatName)
		}
	}
	if provider == "" {
		return "unknown"
	}
	return provider
}

// GetAPIKeyUsage returns recent request buckets for all in-memory api_key auths,
// grouped by provider and keyed by "base_url|api_key".
func (h *Handler) GetAPIKeyUsage(c *gin.Context) {
	if h == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "handler not initialized"})
		return
	}

	h.mu.Lock()
	manager := h.authManager
	h.mu.Unlock()
	if manager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}

	now := time.Now()
	withState := c.GetBool(ConfigV8ContextKey)
	out := make(map[string]map[string]apiKeyUsageEntry)
	for _, auth := range manager.List() {
		if auth == nil {
			continue
		}
		kind, apiKey := auth.AccountInfo()
		if !strings.EqualFold(strings.TrimSpace(kind), "api_key") {
			continue
		}
		apiKey = strings.TrimSpace(apiKey)
		if apiKey == "" {
			continue
		}
		baseURL := ""
		if auth.Attributes != nil {
			baseURL = strings.TrimSpace(auth.Attributes["base_url"])
			if baseURL == "" {
				baseURL = strings.TrimSpace(auth.Attributes["base-url"])
			}
		}
		compositeKey := baseURL + "|" + apiKey
		provider := apiKeyUsageProviderKey(auth)

		recent := auth.RecentRequestsSnapshot(now)
		var state *modelrouting.InventoryCredential
		if withState {
			state = apiKeyUsageState(manager, auth, now)
		}
		providerBucket, ok := out[provider]
		if !ok {
			providerBucket = make(map[string]apiKeyUsageEntry)
			out[provider] = providerBucket
		}
		if existing, exists := providerBucket[compositeKey]; exists {
			existing.Success += auth.Success
			existing.Failed += auth.Failed
			existing.RecentRequests = mergeRecentRequestBuckets(existing.RecentRequests, recent)
			// Registrations sharing one key report the most restrictive state.
			if state != nil && existing.State != nil && existing.State.Health.Selectable && !state.Health.Selectable {
				existing.State = state
			}
			providerBucket[compositeKey] = existing
			continue
		}
		providerBucket[compositeKey] = apiKeyUsageEntry{
			Success:        auth.Success,
			Failed:         auth.Failed,
			RecentRequests: recent,
			State:          state,
		}
	}

	c.JSON(http.StatusOK, out)
}
