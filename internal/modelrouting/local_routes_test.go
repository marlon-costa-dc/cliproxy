package modelrouting

import (
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/ratelimit"
	"gopkg.in/yaml.v3"
)

func TestLocalRouteRateLimitRoundTripsAndValidates(t *testing.T) {
	t.Parallel()
	cfg := validConfig()
	cfg.LocalRoutes = []LocalRoute{{Name: "local-lane", RuntimeModelID: "gpt-5.4", RateLimit: &ratelimit.Spec{MaxConcurrency: 1, DailyMaxRequests: 10}}}
	refreshProjectionDigest(t, &cfg)
	payload, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Config
	if err = yaml.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("Unmarshal() with local-routes: %v", err)
	}
	if err = decoded.Validate(); err != nil {
		t.Fatalf("Validate() with local-routes: %v", err)
	}
	if len(decoded.LocalRoutes) != 1 || decoded.LocalRoutes[0].RateLimit == nil || *decoded.LocalRoutes[0].RateLimit != *cfg.LocalRoutes[0].RateLimit {
		t.Fatalf("round-tripped local-routes = %+v", decoded.LocalRoutes)
	}
	if !strings.Contains(string(SchemaJSON()), `"local-routes":{"items":{"$ref":"#/$defs/local-route"}`) {
		t.Fatal("schema does not declare local-routes")
	}
}

func TestValidateRejectsMalformedLocalRoutes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		route LocalRoute
		want  string
	}{
		{"empty name", LocalRoute{RuntimeModelID: "gpt-5.4"}, "local-routes[0].name"},
		{"empty runtime model", LocalRoute{Name: "local-lane"}, "local-routes[0].runtime-model-id"},
		{"duplicates alias", LocalRoute{Name: "AIHUB-PRIMARY", RuntimeModelID: "gpt-5.4"}, "duplicates another alias"},
		{"negative concurrency", LocalRoute{Name: "local-lane", RuntimeModelID: "gpt-5.4", RateLimit: &ratelimit.Spec{MaxConcurrency: -1}}, "local-routes[0].rate-limit.max-concurrency"},
		{"bad interval", LocalRoute{Name: "local-lane", RuntimeModelID: "gpt-5.4", RateLimit: &ratelimit.Spec{MinInterval: "soon"}}, "local-routes[0].rate-limit.min-interval"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := validConfig()
			cfg.LocalRoutes = []LocalRoute{tc.route}
			refreshProjectionDigest(t, &cfg)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestUnmarshalRejectsUnknownLocalRouteField(t *testing.T) {
	t.Parallel()
	var cfg Config
	err := yaml.Unmarshal([]byte("schema-version: 3\ngeneration: 1\nsnapshot-digest: "+testDigest+"\nprojection-digest: "+testDigest+"\naliases: []\ndirect-models: []\nfailure-policy:\n  mode: classified_candidate_failover\n  credential-acquisition-timeout-seconds: 30\n  automatic-retry: false\n  automatic-failover: true\n  failover-rules: []\n  serve-stale-on-error: false\n  preserve-first-error: true\n  terminate-owned-request-on-cancel: true\nlocal-routes:\n  - name: local-lane\n    runtime-model-id: gpt-5.4\n    rate-limit:\n      requests-per-minute: 5\n"), &cfg)
	if err == nil || !strings.Contains(err.Error(), "local-routes[0].rate-limit.requests-per-minute: unknown field") {
		t.Fatalf("Unmarshal() error = %v, want unknown local-route rate-limit field rejection", err)
	}
}
