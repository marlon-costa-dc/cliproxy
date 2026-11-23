package auth

import (
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

func TestOpenCodeUsageLimitBlocksOnlyItsPoolUntilReset(t *testing.T) {
	withQuotaCooldownEnabled(t)
	for _, tc := range []struct{ provider, code, reporter, peer, other string }{
		{"opencode-go", ErrorCodeOpenCodeSubscriptionLimit, "sub-a", "sub-b", "free-a"},
		{"opencode-go", ErrorCodeOpenCodeFreeLimit, "free-a", "free-b", "sub-a"},
		{"opencode", ErrorCodeOpenCodeFreeLimit, "free-a", "free-b", "sub-a"},
	} {
		t.Run(tc.provider+"/"+tc.code, func(t *testing.T) {
			m := NewManager(nil, nil, nil)
			auth, err := m.Register(t.Context(), &Auth{ID: t.Name(), Provider: tc.provider, QuotaDomain: "workspace", Status: StatusActive})
			if err != nil {
				t.Fatal(err)
			}
			r := registry.GetGlobalRegistry()
			r.RegisterClient(auth.ID, tc.provider, []*registry.ModelInfo{{ID: "sub-a"}, {ID: "sub-b"}, {ID: "free-a", FreeTextQuota: true}, {ID: "free-b", FreeTextQuota: true}})
			t.Cleanup(func() { r.UnregisterClient(auth.ID) })
			retry := time.Hour
			if err := m.MarkResult(t.Context(), Result{AuthID: auth.ID, Provider: tc.provider, Model: tc.reporter, Error: &Error{Code: tc.code, HTTPStatus: http.StatusTooManyRequests}, RetryAfter: &retry}); err != nil {
				t.Fatal(err)
			}
			auth, _ = m.GetByID(auth.ID)
			now := time.Now()
			original := AvailabilityForModel(auth, tc.reporter, now)
			peer := AvailabilityForModel(auth, tc.peer, now)
			if !original.QuotaCooldown || peer != original || !original.RecoverAt.After(now) {
				t.Fatalf("unexpected pool cooldown: reporter=%+v peer=%+v", original, peer)
			}
			if AvailabilityForModel(auth, tc.other, now).Blocked || AvailabilityForModel(auth, tc.peer, peer.RecoverAt).Blocked {
				t.Fatal("other pool or recovered peer remains blocked")
			}
		})
	}
}
