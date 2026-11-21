package auth

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/ratelimit"
)

func credentialRateLimitKey(authID string) string { return "credential:" + authID }

func routeRateLimitKey(selector string) string { return "route:" + selector }

// routeRateLimit returns the published limit of the route a model-routing
// request executes, keyed by its route selector.
func (m *Manager) routeRateLimit(opts cliproxyexecutor.Options) (string, ratelimit.Config, bool) {
	if m == nil || !isModelRoutingOptions(opts) {
		return "", ratelimit.Config{}, false
	}
	selector, _ := opts.Metadata[routingSelectorMetadataKey].(string)
	table := m.modelRouting.Load()
	if selector == "" || table == nil {
		return "", ratelimit.Config{}, false
	}
	limit, ok := table.rateLimits[selector]
	return selector, limit, ok
}

// acquireRateLimits admits one upstream request for the published route and
// then for the credential, in that order, so concurrent requests never wait on
// each other's slots. Waiting queues the request; a reached daily cap of the
// credential refuses only that credential for this request, while a reached
// route cap ends the request. The returned function ends the request and
// records its outcome for the failure backoff; it is nil when no limit applies,
// so an unlimited request keeps its exact execution path.
func (m *Manager) acquireRateLimits(ctx context.Context, auth *Auth, opts cliproxyexecutor.Options) (func(error), error) {
	if m == nil || m.rateLimiter == nil || auth == nil {
		return nil, nil
	}
	type scope struct {
		key string
		cfg ratelimit.Config
	}
	scopes := make([]scope, 0, 2)
	if selector, limit, ok := m.routeRateLimit(opts); ok {
		scopes = append(scopes, scope{key: routeRateLimitKey(selector), cfg: limit})
	}
	if auth.RateLimit != nil && auth.RateLimit.Active() {
		scopes = append(scopes, scope{key: credentialRateLimitKey(auth.ID), cfg: *auth.RateLimit})
	}
	if len(scopes) == 0 {
		return nil, nil
	}
	releases := make([]func(), 0, len(scopes))
	releaseAll := func() {
		for index := len(releases) - 1; index >= 0; index-- {
			releases[index]()
		}
	}
	for _, current := range scopes {
		release, errAcquire := m.rateLimiter.Acquire(ctx, current.key, current.cfg)
		if errAcquire != nil {
			releaseAll()
			return nil, rateLimitAcquireError(current.key == credentialRateLimitKey(auth.ID), errAcquire)
		}
		releases = append(releases, release)
	}
	return func(errExec error) {
		releaseAll()
		for _, current := range scopes {
			switch {
			case errExec == nil:
				m.rateLimiter.MarkSuccess(current.key)
			case !errors.Is(errExec, context.Canceled) && !errors.Is(errExec, context.DeadlineExceeded):
				m.rateLimiter.MarkFailure(current.key, current.cfg)
			}
		}
	}, nil
}

// rateLimitAcquireError classifies a refused admission. Waiting that ended
// with the request context reports the context error unchanged.
func rateLimitAcquireError(credential bool, errAcquire error) error {
	var dailyErr *ratelimit.DailyLimitError
	if !errors.As(errAcquire, &dailyErr) {
		return errAcquire
	}
	retryAfter := int(time.Until(dailyErr.ResetAt).Seconds()) + 1
	header := http.Header{"Retry-After": []string{strconv.Itoa(retryAfter)}}
	if credential {
		return &credentialTerminatedError{terminal: &cliproxyexecutor.RequestTerminatedError{
			HTTPStatus: http.StatusTooManyRequests,
			Header:     header,
			Body:       []byte(`{"error":{"code":"credential_rate_limited","message":"credential reached its local daily request limit"}}`),
			Trusted:    true,
		}}
	}
	return &Error{Code: "route_rate_limited", Message: "published route reached its local daily request limit", HTTPStatus: http.StatusTooManyRequests}
}

// releaseRateLimitsWhenStreamEnds holds the admitted slots until the stream
// finishes or the request context ends.
func releaseRateLimitsWhenStreamEnds(ctx context.Context, result *cliproxyexecutor.StreamResult, release func(error)) *cliproxyexecutor.StreamResult {
	if result == nil || result.Chunks == nil {
		release(nil)
		return result
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		var streamErr error
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				streamErr = chunk.Err
			}
			select {
			case out <- chunk:
			case <-ctx.Done():
				release(ctx.Err())
				for range result.Chunks {
				}
				return
			}
		}
		release(streamErr)
	}()
	wrapped := *result
	wrapped.Chunks = out
	return &wrapped
}

// CredentialRateLimitState reports the pacing state of a credential's limit.
func (m *Manager) CredentialRateLimitState(auth *Auth) (ratelimit.State, bool) {
	if m == nil || m.rateLimiter == nil || auth == nil || auth.RateLimit == nil {
		return ratelimit.State{}, false
	}
	state, _ := m.rateLimiter.Snapshot(credentialRateLimitKey(auth.ID), *auth.RateLimit)
	return state, true
}

// RouteRateLimitState reports the pacing state of a published route's limit.
func (m *Manager) RouteRateLimitState(selector string) (ratelimit.State, bool) {
	if m == nil || m.rateLimiter == nil {
		return ratelimit.State{}, false
	}
	table := m.modelRouting.Load()
	if table == nil {
		return ratelimit.State{}, false
	}
	limit, ok := table.rateLimits[selector]
	if !ok {
		return ratelimit.State{}, false
	}
	state, _ := m.rateLimiter.Snapshot(routeRateLimitKey(selector), limit)
	return state, true
}
