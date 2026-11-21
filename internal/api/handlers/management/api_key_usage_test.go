package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelrouting"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func sumRecentRequestBuckets(buckets []coreauth.RecentRequestBucket) (int64, int64) {
	var success int64
	var failed int64
	for _, bucket := range buckets {
		success += bucket.Success
		failed += bucket.Failed
	}
	return success, failed
}

func TestGetAPIKeyUsage_GroupsByProviderAndAPIKey(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "codex-auth",
		Provider: "codex",
		Attributes: map[string]string{
			"api_key":  "codex-key",
			"base_url": "https://codex.example.com",
		},
	}); err != nil {
		t.Fatalf("register codex auth: %v", err)
	}
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "claude-auth",
		Provider: "claude",
		Attributes: map[string]string{
			"api_key":  "claude-key",
			"base_url": "https://claude.example.com",
		},
	}); err != nil {
		t.Fatalf("register claude auth: %v", err)
	}

	if errMark := manager.MarkResult(context.Background(), coreauth.Result{AuthID: "codex-auth", Provider: "codex", Model: "gpt-5", Success: true}); errMark != nil {
		t.Fatalf("mark codex success: %v", errMark)
	}
	if errMark := manager.MarkResult(context.Background(), coreauth.Result{AuthID: "codex-auth", Provider: "codex", Model: "gpt-5", Success: false}); errMark != nil {
		t.Fatalf("mark codex failure: %v", errMark)
	}
	if errMark := manager.MarkResult(context.Background(), coreauth.Result{AuthID: "claude-auth", Provider: "claude", Model: "claude-4", Success: true}); errMark != nil {
		t.Fatalf("mark claude success: %v", errMark)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/api-key-usage", nil)
	ginCtx.Request = req
	h.GetAPIKeyUsage(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload map[string]map[string]apiKeyUsageEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	codexEntry := payload["codex"]["https://codex.example.com|codex-key"]
	if codexEntry.Success != 1 || codexEntry.Failed != 1 {
		t.Fatalf("codex totals = %d/%d, want 1/1", codexEntry.Success, codexEntry.Failed)
	}
	if len(codexEntry.RecentRequests) != 20 {
		t.Fatalf("codex buckets len = %d, want 20", len(codexEntry.RecentRequests))
	}
	codexSuccess, codexFailed := sumRecentRequestBuckets(codexEntry.RecentRequests)
	if codexSuccess != 1 || codexFailed != 1 {
		t.Fatalf("codex totals = %d/%d, want 1/1", codexSuccess, codexFailed)
	}

	claudeEntry := payload["claude"]["https://claude.example.com|claude-key"]
	if claudeEntry.Success != 1 || claudeEntry.Failed != 0 {
		t.Fatalf("claude totals = %d/%d, want 1/0", claudeEntry.Success, claudeEntry.Failed)
	}
	if len(claudeEntry.RecentRequests) != 20 {
		t.Fatalf("claude buckets len = %d, want 20", len(claudeEntry.RecentRequests))
	}
	claudeSuccess, claudeFailed := sumRecentRequestBuckets(claudeEntry.RecentRequests)
	if claudeSuccess != 1 || claudeFailed != 0 {
		t.Fatalf("claude totals = %d/%d, want 1/0", claudeSuccess, claudeFailed)
	}
}

func TestGetAPIKeyUsage_GroupsOpenAICompatibleByCompatName(t *testing.T) {
	t.Setenv("MANAGEMENT_PASSWORD", "")

	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{
		ID:       "vast-auth",
		Provider: "openai-compatible-vast",
		Attributes: map[string]string{
			"api_key":     "vast-key",
			"base_url":    "https://www.vastnum.com/v1",
			"compat_name": "VAST",
		},
	}); err != nil {
		t.Fatalf("register vast auth: %v", err)
	}

	if errMark := manager.MarkResult(context.Background(), coreauth.Result{AuthID: "vast-auth", Provider: "openai-compatible-vast", Model: "gpt-5", Success: true}); errMark != nil {
		t.Fatalf("mark VAST success: %v", errMark)
	}

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)

	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodGet, "/v0/management/api-key-usage", nil)
	ginCtx.Request = req
	h.GetAPIKeyUsage(ginCtx)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rec.Code, http.StatusOK, rec.Body.String())
	}

	var payload map[string]map[string]apiKeyUsageEntry
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}

	if _, exists := payload["openai-compatible-vast"]; exists {
		t.Fatalf("unexpected namespaced provider bucket in payload: %#v", payload)
	}
	vastBucket, exists := payload["vast"]
	if !exists {
		t.Fatalf("missing compat provider bucket in payload: %#v", payload)
	}
	vastEntry := vastBucket["https://www.vastnum.com/v1|vast-key"]
	if vastEntry.Success != 1 || vastEntry.Failed != 0 {
		t.Fatalf("vast totals = %d/%d, want 1/0", vastEntry.Success, vastEntry.Failed)
	}
}

func TestGetAPIKeyUsageV8ReportsOpenCodeKeyRuntimeState(t *testing.T) {
	now := time.Now()
	providerText := "upstream said: account abc123 is throttled"
	manager := coreauth.NewManager(nil, nil, nil)
	for _, auth := range []*coreauth.Auth{
		{
			ID: "opencode-healthy", Provider: "opencode", QuotaDomain: "zen-team",
			Attributes: map[string]string{"api_key": "zen-healthy"},
		},
		{
			ID: "opencode-cooldown", Provider: "opencode",
			Attributes:  map[string]string{"api_key": "zen-cooldown"},
			Unavailable: true, NextRetryAfter: now.Add(5 * time.Minute),
			StatusMessage: providerText, LastError: &coreauth.Error{Message: providerText, HTTPStatus: http.StatusServiceUnavailable},
		},
		{
			ID: "opencode-go-quota", Provider: "opencode-go",
			Attributes: map[string]string{"api_key": "go-quota"},
			Quota:      coreauth.QuotaState{Exceeded: true, Reason: "quota", NextRecoverAt: now.Add(10 * time.Minute)},
		},
		{
			ID: "opencode-go-disabled", Provider: "opencode-go", Disabled: true, Status: coreauth.StatusDisabled,
			Attributes: map[string]string{"api_key": "go-disabled"},
		},
	} {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", auth.ID, errRegister)
		}
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, manager)
	router := gin.New()
	router.GET("/v8/management/observability/usage/api-keys", func(c *gin.Context) {
		c.Set(ConfigV8ContextKey, true)
		h.GetAPIKeyUsage(c)
	})
	router.GET("/v0/management/api-key-usage", h.GetAPIKeyUsage)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v8/management/observability/usage/api-keys", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "abc123") || strings.Contains(rec.Body.String(), "throttled") {
		t.Fatalf("provider error text crossed the management boundary: %s", rec.Body.String())
	}
	var payload map[string]map[string]apiKeyUsageEntry
	if errDecode := json.Unmarshal(rec.Body.Bytes(), &payload); errDecode != nil {
		t.Fatalf("decode payload: %v", errDecode)
	}
	state := func(provider, key string) *modelrouting.InventoryCredential {
		t.Helper()
		entry, ok := payload[provider]["|"+key]
		if !ok || entry.State == nil {
			t.Fatalf("missing %s state for %s: %v", provider, key, payload)
		}
		return entry.State
	}

	healthy := state("opencode", "zen-healthy")
	if !healthy.Health.Selectable || healthy.Suspension.Active || healthy.Quota.Status != "available" || healthy.QuotaDomain != "zen-team" {
		t.Fatalf("healthy state = %+v", healthy)
	}
	if healthy.CredentialRef.ID != coreauth.CredentialReferenceID("opencode-healthy") || healthy.CredentialRef.Kind != "api_key" {
		t.Fatalf("healthy credential ref = %+v", healthy.CredentialRef)
	}

	cooldown := state("opencode", "zen-cooldown")
	if cooldown.Health.Selectable || !cooldown.Suspension.Active || cooldown.Suspension.Reason == nil || *cooldown.Suspension.Reason != "cooldown" {
		t.Fatalf("cooldown state = %+v", cooldown)
	}
	if cooldown.Suspension.ResumesAt == nil || *cooldown.Suspension.ResumesAt != now.Add(5*time.Minute).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("cooldown resume instant = %v", cooldown.Suspension.ResumesAt)
	}
	if cooldown.QuotaDomain != "opencode" {
		t.Fatalf("cooldown quota domain = %q, want provider pool", cooldown.QuotaDomain)
	}

	quota := state("opencode-go", "go-quota")
	if quota.Health.Selectable || quota.Suspension.Active || quota.Quota.Status != "blocked" || quota.Quota.ResetsAt == nil {
		t.Fatalf("quota state = %+v", quota)
	}
	if *quota.Quota.ResetsAt != now.Add(10*time.Minute).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("quota resets_at = %s", *quota.Quota.ResetsAt)
	}

	disabled := state("opencode-go", "go-disabled")
	if disabled.Health.Selectable || disabled.Suspension.Reason == nil || *disabled.Suspension.Reason != "disabled" {
		t.Fatalf("disabled state = %+v", disabled)
	}

	legacy := httptest.NewRecorder()
	router.ServeHTTP(legacy, httptest.NewRequest(http.MethodGet, "/v0/management/api-key-usage", nil))
	if legacy.Code != http.StatusOK || strings.Contains(legacy.Body.String(), `"state"`) {
		t.Fatalf("v0 payload changed: status=%d body=%s", legacy.Code, legacy.Body.String())
	}
}
