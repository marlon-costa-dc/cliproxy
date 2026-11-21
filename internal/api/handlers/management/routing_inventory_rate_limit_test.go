package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelrouting"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/ratelimit"
)

func TestRoutingInventoryReportsCredentialRateLimit(t *testing.T) {
	manager := coreauth.NewManager(nil, nil, nil)
	limited := &coreauth.Auth{ID: "opencode-limited", Provider: "opencode", Attributes: map[string]string{"api_key": "zen-limited"},
		RateLimit: &ratelimit.Config{MinInterval: time.Second, DailyMaxRequests: 100, MaxConcurrency: 1}}
	open := &coreauth.Auth{ID: "opencode-open", Provider: "opencode", Attributes: map[string]string{"api_key": "zen-open"}}
	for _, auth := range []*coreauth.Auth{limited, open} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
	}
	now := time.Now()
	inventory := modelrouting.Inventory{DirectModels: []modelrouting.InventoryModel{{Routes: []modelrouting.InventoryRoute{{
		RouteSelector: "sha256:unpublished",
		Credentials: []modelrouting.InventoryCredential{
			{CredentialRef: modelrouting.InventoryCredentialRef{ID: coreauth.CredentialReferenceID(limited.ID)}},
			{CredentialRef: modelrouting.InventoryCredentialRef{ID: coreauth.CredentialReferenceID(open.ID)}},
		},
	}}}}}
	annotateInventoryRateLimits(&inventory, manager, map[string]*coreauth.Auth{limited.ID: limited, open.ID: open}, now)

	route := inventory.DirectModels[0].Routes[0]
	if route.RateLimit != nil {
		t.Fatalf("route without a published limit reported %+v", route.RateLimit)
	}
	got := route.Credentials[0].RateLimit
	if got == nil || got.MaxConcurrency != 1 || got.DailyMaxRequests != 100 || got.MinIntervalMS != 1000 || got.InFlight != 0 || got.DailyRequests != 0 {
		t.Fatalf("limited credential rate limit = %+v", got)
	}
	if route.Credentials[1].RateLimit != nil {
		t.Fatalf("unlimited credential reported %+v", route.Credentials[1].RateLimit)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	router := gin.New()
	router.GET("/v8/management/observability/usage/api-keys", func(c *gin.Context) {
		c.Set(ConfigV8ContextKey, true)
		h.GetAPIKeyUsage(c)
	})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/api-keys", nil))
	var payload map[string]map[string]apiKeyUsageEntry
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode: %v body=%s", errDecode, rec.Body.String())
	}
	if state := payload["opencode"]["|zen-limited"].State; state == nil || state.RateLimit == nil || state.RateLimit.MaxConcurrency != 1 {
		t.Fatalf("api-key usage state = %+v", state)
	}
}
