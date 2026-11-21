package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelrouting"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/ratelimit"
)

// newRateLimitedCredentials registers credentials of one provider, each with
// the given limit, and returns the manager, model and executor.
func newRateLimitedCredentials(t *testing.T, limit ratelimit.Config, count int) (*Manager, string, *modelRoutingAttemptExecutor, []string) {
	t.Helper()
	provider := "rate-limit-" + uuid.NewString()
	model := "rate-limit-model-" + uuid.NewString()
	manager := NewManager(nil, nil, NoopHook{})
	manager.SetRetryConfig(0, time.Second, 0)
	executor := &modelRoutingAttemptExecutor{identifier: provider}
	manager.RegisterExecutor(executor)
	ids := make([]string, 0, count)
	for range count {
		id := "credential-" + uuid.NewString()
		cfg := limit
		if _, errRegister := manager.Register(context.Background(), &Auth{ID: id, Provider: provider, Status: StatusActive, RateLimit: &cfg}); errRegister != nil {
			t.Fatalf("Register() error = %v", errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(id, provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		manager.RefreshSchedulerEntry(id)
		ids = append(ids, id)
	}
	return manager, model, executor, ids
}

func TestExecuteQueuesRequestsAtCredentialMaxConcurrencyOne(t *testing.T) {
	manager, model, executor, ids := newRateLimitedCredentials(t, ratelimit.Config{MaxConcurrency: 1}, 1)
	var inFlight, peak atomic.Int32
	executor.executeFn = func(context.Context, *Auth) (cliproxyexecutor.Response, error) {
		current := inFlight.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		inFlight.Add(-1)
		return cliproxyexecutor.Response{Payload: []byte(`{"choices":[{"message":{"content":"ok"}}]}`)}, nil
	}

	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if _, errExecute := manager.Execute(context.Background(), []string{executor.identifier}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{}); errExecute != nil {
				t.Errorf("Execute() error = %v, want the request queued and served", errExecute)
			}
		})
	}
	wg.Wait()
	if got := executor.executeCalls.Load(); got != 5 {
		t.Fatalf("upstream calls = %d, want 5", got)
	}
	if got := peak.Load(); got != 1 {
		t.Fatalf("peak upstream concurrency = %d, want 1", got)
	}
	auth, _ := manager.GetByID(ids[0])
	state, ok := manager.CredentialRateLimitState(auth)
	if !ok || state.InFlight != 0 || state.Waiting != 0 || state.DailyRequests != 5 || state.Config.MaxConcurrency != 1 {
		t.Fatalf("CredentialRateLimitState() = %+v, %t", state, ok)
	}
}

func TestExecuteStreamHoldsCredentialSlotUntilStreamEnds(t *testing.T) {
	manager, model, executor, _ := newRateLimitedCredentials(t, ratelimit.Config{MaxConcurrency: 1}, 1)
	firstChunks := make(chan cliproxyexecutor.StreamChunk, 1)
	firstChunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"choices":[{"delta":{"content":"first"}}]}`)}
	var calls atomic.Int32
	executor.streamFn = func(context.Context) (*cliproxyexecutor.StreamResult, error) {
		if calls.Add(1) == 1 {
			return &cliproxyexecutor.StreamResult{Chunks: firstChunks}, nil
		}
		chunks := make(chan cliproxyexecutor.StreamChunk, 1)
		chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`{"choices":[{"delta":{"content":"second"}}]}`)}
		close(chunks)
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}
	request := cliproxyexecutor.Request{Model: model}
	first, errFirst := manager.ExecuteStream(context.Background(), []string{executor.identifier}, request, cliproxyexecutor.Options{})
	if errFirst != nil {
		t.Fatalf("first ExecuteStream() error = %v", errFirst)
	}

	secondDone := make(chan error, 1)
	go func() {
		second, errSecond := manager.ExecuteStream(context.Background(), []string{executor.identifier}, request, cliproxyexecutor.Options{})
		if errSecond == nil {
			for range second.Chunks {
			}
		}
		secondDone <- errSecond
	}()
	time.Sleep(50 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("second stream reached upstream (%d calls) while the first held the only slot", got)
	}

	close(firstChunks)
	for range first.Chunks {
	}
	select {
	case errSecond := <-secondDone:
		if errSecond != nil {
			t.Fatalf("second ExecuteStream() error = %v", errSecond)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second stream stayed queued after the first ended")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream stream calls = %d, want 2", got)
	}
}

func TestExecuteRefusesCredentialAtDailyCapAndUsesAnother(t *testing.T) {
	manager, model, executor, _ := newRateLimitedCredentials(t, ratelimit.Config{DailyMaxRequests: 1}, 2)
	request := cliproxyexecutor.Request{Model: model}
	for attempt := range 2 {
		if _, errExecute := manager.Execute(context.Background(), []string{executor.identifier}, request, cliproxyexecutor.Options{}); errExecute != nil {
			t.Fatalf("request %d error = %v, want a credential under its cap", attempt+1, errExecute)
		}
	}
	_, errExecute := manager.Execute(context.Background(), []string{executor.identifier}, request, cliproxyexecutor.Options{})
	var terminated *cliproxyexecutor.RequestTerminatedError
	if !errors.As(errExecute, &terminated) || terminated.HTTPStatus != http.StatusTooManyRequests || terminated.Header.Get("Retry-After") == "" || !terminated.Trusted {
		t.Fatalf("third request error = %v, want a 429 local daily-cap refusal", errExecute)
	}
	if got := executor.executeCalls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (the capped request never reaches upstream)", got)
	}
}

func TestModelRoutingRouteDailyCapFailsOverToNextRoute(t *testing.T) {
	primary := &modelRoutingAttemptExecutor{identifier: "route-limited-" + uuid.NewString()}
	healthy := &modelRoutingAttemptExecutor{identifier: "route-open-" + uuid.NewString()}
	runtime := newModelRoutingTestRuntime(t, primary, healthy, 2)

	projection := *runtime.projection
	projection.DirectModels = append([]modelrouting.DirectModel(nil), projection.DirectModels...)
	projection.DirectModels[0].Routes = append([]modelrouting.DirectRoute(nil), projection.DirectModels[0].Routes...)
	projection.DirectModels[0].Routes[0].RateLimit = &ratelimit.Spec{DailyMaxRequests: 1}
	digest, errDigest := modelrouting.ProjectionDigest(&projection)
	if errDigest != nil {
		t.Fatal(errDigest)
	}
	projection.ProjectionDigest = digest
	prepared, errPrepare := runtime.manager.PrepareModelRouting(&projection)
	if errPrepare != nil {
		t.Fatalf("PrepareModelRouting() error = %v", errPrepare)
	}
	if errActivate := runtime.manager.ActivatePreparedModelRouting(prepared); errActivate != nil {
		t.Fatalf("ActivatePreparedModelRouting() error = %v", errActivate)
	}

	for range 2 {
		if _, errExecute := runtime.manager.Execute(context.Background(), []string{primary.identifier, healthy.identifier},
			cliproxyexecutor.Request{Model: runtime.requestedModel, Payload: []byte(`{"messages":[]}`)}, modelRoutingOptions()); errExecute != nil {
			t.Fatalf("Execute() error = %v", errExecute)
		}
	}
	if got := primary.executeCalls.Load(); got != 1 {
		t.Fatalf("limited route calls = %d, want 1 (its daily cap is 1)", got)
	}
	if got := healthy.executeCalls.Load(); got != 1 {
		t.Fatalf("next route calls = %d, want 1 after the capped route", got)
	}
	selector := projection.DirectModels[0].Routes[0].RouteSelector
	state, ok := runtime.manager.RouteRateLimitState(selector)
	if !ok || state.DailyRequests != 1 || state.Config.DailyMaxRequests != 1 {
		t.Fatalf("RouteRateLimitState() = %+v, %t", state, ok)
	}
}
