package auth

import (
	"context"
	"errors"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestRequestTerminatedErrorSkipsCreditsFallback(t *testing.T) {
	errTerminated := &cliproxyexecutor.RequestTerminatedError{HTTPStatus: http.StatusTooManyRequests}
	if !isRequestTerminatedError(errTerminated) {
		t.Fatal("isRequestTerminatedError() = false")
	}
	if shouldAttemptAntigravityCreditsFallback(&Manager{}, errTerminated, []string{"antigravity"}) {
		t.Fatal("terminated request must not use Antigravity credits fallback")
	}
}

func TestRequestTerminatedErrorZeroValueIsUntrustedAndApplyAfterAuthSetsTrusted(t *testing.T) {
	// Zero-value Trusted must be false (safe default for untrusted upstreams).
	zero := &cliproxyexecutor.RequestTerminatedError{HTTPStatus: http.StatusBadGateway}
	if zero.Trusted {
		t.Fatal("zero-value RequestTerminatedError.Trusted must be false")
	}

	// A termination produced by the after-auth interceptor must be trusted.
	executor := &afterAuthTestExecutor{id: "afterauth"}
	appliedTerminate := false
	interceptor := func(context.Context, cliproxyexecutor.RequestAfterAuthInterceptRequest) cliproxyexecutor.RequestAfterAuthInterceptResponse {
		appliedTerminate = true
		return cliproxyexecutor.RequestAfterAuthInterceptResponse{
			Terminate:       true,
			StatusCode:      http.StatusTeapot,
			ResponseHeaders: http.Header{"X-AfterAuth": []string{"yes"}},
			ResponseBody:    []byte(`{"ok":true}`),
		}
	}
	_, _, errIntercept := prepareRequestAfterAuth(context.Background(), executor, &Auth{ID: "auth-a", Provider: "afterauth"}, "afterauth", cliproxyexecutor.Request{}, cliproxyexecutor.Options{
		RequestAfterAuthInterceptor: interceptor,
	}, "model")
	var terminated *cliproxyexecutor.RequestTerminatedError
	if !errors.As(errIntercept, &terminated) || terminated == nil {
		t.Fatalf("prepareRequestAfterAuth error = %v, want RequestTerminatedError", errIntercept)
	}
	if !terminated.Trusted {
		t.Fatal("after-auth interceptor termination must set Trusted=true")
	}
	if terminated.HTTPStatus != http.StatusTeapot {
		t.Fatalf("terminated status = %d, want %d", terminated.HTTPStatus, http.StatusTeapot)
	}
	if !appliedTerminate {
		t.Fatal("interceptor was not applied")
	}
}
