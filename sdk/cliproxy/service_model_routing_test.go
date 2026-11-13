package cliproxy

import (
	"os"
	"path/filepath"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/modelrouting"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// A persisted projection whose credential no longer exists (a rotated key)
// must not stop the proxy: it starts with no active generation, so the
// publisher can bootstrap a fresh one.
func TestInitializeModelRoutingStartsWithoutAStaleProjection(t *testing.T) {
	modelKey := modelrouting.ModelKey{CatalogProviderID: "catalog-provider", CanonicalModelID: "rotated-model"}
	routeKey := modelrouting.RouteKey{ModelKey: modelKey, RouteChannel: "rotated-channel"}
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("port: 8317\n"), 0o600); errWrite != nil {
		t.Fatalf("write config: %v", errWrite)
	}
	service := &Service{
		coreManager: coreauth.NewManager(nil, nil, coreauth.NoopHook{}),
		configPath:  configPath,
		cfg: &internalconfig.Config{ModelRouting: &modelrouting.Config{
			Generation: 11,
			DirectModels: []modelrouting.DirectModel{{
				ModelKey: modelKey, DisplayName: "rotated-model", Active: true,
				Routes: []modelrouting.DirectRoute{{
					RouteKey: routeKey, CatalogRouteProviderID: "rotated-channel", CatalogRouteModelID: "rotated-model",
					RuntimeModelID: "rotated-model", RouteSelector: modelrouting.SelectorForRoute(routeKey, "rotated-model"),
					QuotaDomains:   []string{"rotated-quota"},
					CredentialRefs: []modelrouting.CredentialRef{{ID: coreauth.CredentialReferenceID("key-before-rotation"), Kind: "api_key"}},
					Protocols:      []string{"openai_chat"},
					Health:         modelrouting.Health{Status: "healthy", Selectable: true},
					Selectable:     true,
				}},
			}},
		}},
	}

	if errInit := service.initializeModelRouting(); errInit != nil {
		t.Fatalf("initializeModelRouting with a stale projection = %v, want a started service", errInit)
	}
	if identity := service.modelRoutingIdentity(); identity != nil {
		t.Fatalf("active model-routing identity = %+v, want none", identity)
	}
	if service.cfg.ModelRouting != nil {
		t.Fatal("stale projection is still part of the running configuration")
	}
}
