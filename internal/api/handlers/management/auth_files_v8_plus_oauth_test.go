package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// stubPlusProviderOAuthTransport answers the device-code start endpoints and
// fails every poll so background login goroutines end without real network use.
func stubPlusProviderOAuthTransport(t *testing.T) {
	t.Helper()
	origTransport := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rec := httptest.NewRecorder()
		rec.Header().Set("Content-Type", "application/json")
		switch {
		case req.URL.Host == "github.com" && req.URL.Path == "/login/device/code":
			_, _ = rec.WriteString(`{"device_code":"gh-device","user_code":"GH-1234","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`)
		case req.URL.Host == "api.kilo.ai" && req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/device-auth/codes"):
			_, _ = rec.WriteString(`{"code":"KILO-5678","verificationUrl":"https://app.kilo.ai/device","expiresIn":600}`)
		case req.URL.Host == "oidc.us-east-1.amazonaws.com" && req.URL.Path == "/client/register":
			_, _ = rec.WriteString(`{"clientId":"kiro-client","clientSecret":"kiro-secret"}`)
		case req.URL.Host == "oidc.us-east-1.amazonaws.com" && req.URL.Path == "/device_authorization":
			_, _ = rec.WriteString(`{"deviceCode":"kiro-device","userCode":"KIRO-ABCD","verificationUri":"https://view.awsapps.com/start","verificationUriComplete":"https://view.awsapps.com/start?user_code=KIRO-ABCD","expiresIn":600,"interval":30}`)
		default:
			rec.WriteHeader(http.StatusBadRequest)
			_, _ = rec.WriteString(`{"error":"access_denied"}`)
		}
		return rec.Result(), nil
	})
	t.Cleanup(func() { http.DefaultTransport = origTransport })
}

func startOAuthV8ForTest(t *testing.T, router http.Handler, method, target, body string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s %s status=%d body=%s", method, target, rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if errUnmarshal := json.Unmarshal(rec.Body.Bytes(), &resp); errUnmarshal != nil {
		t.Fatalf("decode start response: %v", errUnmarshal)
	}
	state, _ := resp["state"].(string)
	if state == "" {
		t.Fatalf("start response has no state: %v", resp)
	}
	t.Cleanup(func() { CancelOAuthSession(state) })
	return resp
}

func TestStartOAuthV8RoutesPlusProviders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stubPlusProviderOAuthTransport(t)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir(), Port: 8317}, coreauth.NewManager(nil, nil, nil))
	router := gin.New()
	router.GET("/v8/management/oauth/auth-url", h.StartOAuthV8)

	for _, tc := range []struct {
		query        string
		session      string
		urlPrefix    string
		flow         string
		userCode     string
		expiresIn    float64
		verification bool
	}{
		{query: "provider=qoder", session: "qoder", flow: "device"},
		{query: "provider=kilo", session: "kilo", urlPrefix: "https://app.kilo.ai/device", flow: "device", userCode: "KILO-5678", expiresIn: 600, verification: true},
		{query: "provider=cursor", session: "cursor"},
		{query: "provider=gitlab&client_id=test-client", session: "gitlab", urlPrefix: "https://gitlab.com/oauth/authorize"},
		{query: "provider=github", session: "github-copilot", urlPrefix: "https://github.com/login/device", flow: "device", userCode: "GH-1234", expiresIn: 900, verification: true},
		{query: "provider=github-copilot", session: "github-copilot", urlPrefix: "https://github.com/login/device", flow: "device", userCode: "GH-1234", expiresIn: 900, verification: true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			resp := startOAuthV8ForTest(t, router, http.MethodGet, "/v8/management/oauth/auth-url?"+tc.query, "")
			state := resp["state"].(string)
			if !IsOAuthSessionPending(state, tc.session) {
				t.Fatalf("session %q is not pending for %s", state, tc.session)
			}
			authURL, _ := resp["url"].(string)
			if authURL == "" || !strings.HasPrefix(authURL, tc.urlPrefix) {
				t.Fatalf("url = %q, want prefix %q", authURL, tc.urlPrefix)
			}
			if got, _ := resp["flow"].(string); got != tc.flow {
				t.Fatalf("flow = %q, want %q", got, tc.flow)
			}
			if got, _ := resp["user_code"].(string); got != tc.userCode {
				t.Fatalf("user_code = %q, want %q", got, tc.userCode)
			}
			if got, _ := resp["expires_in"].(float64); got != tc.expiresIn {
				t.Fatalf("expires_in = %v, want %v", got, tc.expiresIn)
			}
			if got, _ := resp["verification_uri"].(string); tc.verification != (got == authURL) {
				t.Fatalf("verification_uri = %q with url %q", got, authURL)
			}
		})
	}
}

func TestStartOAuthV8PlusProviderBindsTargetedRelogin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stubPlusProviderOAuthTransport(t)

	authDir := t.TempDir()
	fileName := "kilo-target.json"
	meta := map[string]any{"type": "kilo", "email": "a@x.test"}
	raw, _ := json.Marshal(meta)
	fullPath := filepath.Join(authDir, fileName)
	if errWrite := os.WriteFile(fullPath, raw, 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	if _, errRegister := manager.Register(context.Background(), &coreauth.Auth{
		ID: fileName, FileName: fileName, Provider: "kilo", Metadata: meta,
		Attributes: map[string]string{coreauth.AttributePath: fullPath},
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir, Port: 8317}, manager)
	router := gin.New()
	router.POST("/v8/management/oauth/auth-url", h.StartOAuthV8)

	resp := startOAuthV8ForTest(t, router, http.MethodPost, "/v8/management/oauth/auth-url", `{"provider":"kilo","name":"kilo-target.json","verify":true}`)
	if got, _ := resp["target_auth_id"].(string); got != fileName {
		t.Fatalf("target_auth_id = %q, want %q: %v", got, fileName, resp)
	}
	target, ok := GetOAuthSessionTarget(resp["state"].(string))
	if !ok || target.AuthID != fileName || !target.VerifyAfterLogin {
		t.Fatalf("session target = (%+v, %t)", target, ok)
	}
}

func TestStartOAuthV8KiroReturnsLoginURLAndUserCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	stubPlusProviderOAuthTransport(t)

	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir(), Port: 8317}, coreauth.NewManager(nil, nil, nil))
	router := gin.New()
	router.GET("/v8/management/oauth/auth-url", h.StartOAuthV8)
	router.GET("/v8/management/oauth/status", h.GetAuthStatus)

	for _, tc := range []struct {
		query     string
		method    string
		urlPrefix string
		flow      string
		userCode  string
		expiresIn float64
	}{
		{query: "provider=kiro", method: "device_code", urlPrefix: "https://view.awsapps.com/start?user_code=KIRO-ABCD", flow: "device", userCode: "KIRO-ABCD", expiresIn: 600},
		{query: "provider=kiro&method=builder-id", method: "device_code", urlPrefix: "https://view.awsapps.com/start?user_code=KIRO-ABCD", flow: "device", userCode: "KIRO-ABCD", expiresIn: 600},
		{query: "provider=kiro&method=google", method: "social", urlPrefix: "https://prod.us-east-1.auth.desktop.kiro.dev/login?idp=Google&"},
		{query: "provider=kiro&method=github", method: "social", urlPrefix: "https://prod.us-east-1.auth.desktop.kiro.dev/login?idp=Github&"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			resp := startOAuthV8ForTest(t, router, http.MethodGet, "/v8/management/oauth/auth-url?"+tc.query, "")
			state := resp["state"].(string)
			authURL, _ := resp["url"].(string)
			if !strings.HasPrefix(authURL, tc.urlPrefix) {
				t.Fatalf("url = %q, want prefix %q", authURL, tc.urlPrefix)
			}
			if tc.method == "social" && !strings.Contains(authURL, "state="+state) {
				t.Fatalf("social url %q does not carry state %q", authURL, state)
			}
			if got, _ := resp["method"].(string); got != tc.method {
				t.Fatalf("method = %q, want %q", got, tc.method)
			}
			if got, _ := resp["flow"].(string); got != tc.flow {
				t.Fatalf("flow = %q, want %q", got, tc.flow)
			}
			if got, _ := resp["user_code"].(string); got != tc.userCode {
				t.Fatalf("user_code = %q, want %q", got, tc.userCode)
			}
			if got, _ := resp["expires_in"].(float64); got != tc.expiresIn {
				t.Fatalf("expires_in = %v, want %v", got, tc.expiresIn)
			}

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/status?state="+state, nil))
			if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"wait"}` {
				t.Fatalf("pending kiro status = %s, want wait", got)
			}
			if !CancelOAuthSession(state) {
				t.Fatal("pending kiro session was not cancellable")
			}
		})
	}
}
