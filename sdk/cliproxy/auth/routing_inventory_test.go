package auth

import (
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"testing"
	"time"
)

func TestOpenCodeFreeModelQuotaDomain(t *testing.T) {
	for _, tc := range []struct{ provider, gateway, model string }{
		{"opencode", "zen", "big-pickle"}, {"opencode-go", "go", "beam-free"},
	} {
		a := &Auth{Provider: tc.provider, QuotaDomain: "workspace"}
		if a.ModelQuotaDomain(tc.model) == a.ModelQuotaDomain("deepseek-v4-pro") {
			t.Errorf("%s shares the subscription domain", tc.model)
		}
		now := time.Now()
		reset := now.Add(time.Hour)
		a.ModelStates = map[string]*ModelState{"deepseek-v4-pro": {Unavailable: true, NextRetryAfter: reset, Quota: QuotaState{Exceeded: true, NextRecoverAt: reset}}}
		if !AvailabilityForModel(a, "deepseek-v4-pro", now).Blocked || AvailabilityForModel(a, tc.model, now).Blocked {
			t.Errorf("%s should remain available during the subscription model cooldown", tc.model)
		}
		found := false
		for _, m := range registry.GetOpenCodeModels(tc.gateway) {
			if m.ID == tc.model && len(m.Protocols) > 0 {
				found = true
			}
		}
		if !found {
			t.Errorf("%s missing from executable inventory", tc.model)
		}
	}
}
