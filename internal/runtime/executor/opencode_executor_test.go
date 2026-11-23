package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelrouting"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	clipoauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestOpenCodeExecutor_RoutesByProtocol(t *testing.T) {
	cases := []struct {
		name       string
		gateway    string
		modelID    string
		wantPath   string
		wantFormat sdktranslator.Format
		wantErr    bool
	}{
		{"zen-responses", "opencode", "fixture-responses", "/v1/responses", sdktranslator.FormatOpenAIResponse, false},
		{"zen-messages", "opencode", "fixture-messages", "/v1/messages", sdktranslator.FormatClaude, false},
		{"zen-chat", "opencode", "fixture-chat", "/v1/chat/completions", sdktranslator.FormatOpenAI, false},
		{"go-responses", "opencode-go", "fixture-responses", "/v1/responses", sdktranslator.FormatOpenAIResponse, false},
		{"go-messages", "opencode-go", "fixture-messages", "/v1/messages", sdktranslator.FormatClaude, false},
		{"go-chat", "opencode-go", "fixture-chat", "/v1/chat/completions", sdktranslator.FormatOpenAI, false},
		{"unknown-model", "opencode", "no-such-model", "", sdktranslator.FormatOpenAI, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.wantErr {
				protocol := map[string]string{"fixture-responses": modelrouting.ProtocolOpenAIResponses, "fixture-messages": modelrouting.ProtocolAnthropicMessages, "fixture-chat": modelrouting.ProtocolOpenAIChat}[tc.modelID]
				registerOpenCodeTestRoute(t, tc.gateway, tc.modelID, protocol)
			}
			exec := NewOpenCodeExecutor(tc.gateway)
			if exec.Identifier() != tc.gateway {
				t.Fatalf("Identifier() = %q, want %q", exec.Identifier(), tc.gateway)
			}
			if tc.wantErr {
				_, _, err := exec.resolveRoute(tc.modelID)
				if err == nil {
					t.Fatalf("expected error for unknown model, got nil")
				}
				return
			}
			proto, path, err := exec.resolveRoute(tc.modelID)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if proto == "" {
				t.Fatal("expected non-empty protocol")
			}
			if path != tc.wantPath {
				t.Errorf("path = %q, want %q", path, tc.wantPath)
			}
			gotFormat := exec.RequestToFormat(cliproxyexecutor.Request{Model: tc.modelID}, cliproxyexecutor.Options{})
			if gotFormat != tc.wantFormat {
				t.Errorf("format = %q, want %q", gotFormat, tc.wantFormat)
			}
		})
	}
}

func registerOpenCodeTestRoute(t *testing.T, provider, modelID, protocol string) {
	t.Helper()
	clientID := "test-opencode-" + uuid.NewString()
	registry.GetGlobalRegistry().RegisterClient(clientID, provider, []*registry.ModelInfo{{
		ID: modelID, Protocols: []string{protocol},
	}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(clientID) })
}

func TestOpenCodeExecutor_ExecuteDispatchesResponses(t *testing.T) {
	registerOpenCodeTestRoute(t, constant.OpenCode, "fixture-responses", modelrouting.ProtocolOpenAIResponses)
	var capturedPath, bearer, anthropicKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		bearer = r.Header.Get("Authorization")
		anthropicKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"chat.completion","choices":[{"index":0,"message":{"content":"hi","role":"assistant"}}]}`))
	}))
	defer server.Close()

	exec := NewOpenCodeExecutor(constant.OpenCode)
	exec.cfg = &config.Config{}

	auth := &clipoauth.Auth{
		Provider:   constant.OpenCode,
		Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL},
	}
	req := cliproxyexecutor.Request{
		Model:   "fixture-responses",
		Payload: []byte(`{"model":"fixture-responses","messages":[{"role":"user","content":"hi"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}

	resp, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if capturedPath != "/v1/responses" {
		t.Errorf("upstream path = %q, want /v1/responses", capturedPath)
	}
	if bearer != "Bearer test-key" || anthropicKey != "" {
		t.Fatal("Responses transport did not use exclusive Bearer authentication")
	}
	if len(resp.Payload) == 0 {
		t.Error("expected non-empty response payload")
	}
}

func TestOpenCodeExecutor_ExecuteDispatchesMessages(t *testing.T) {
	registerOpenCodeTestRoute(t, constant.OpenCode, "fixture-messages", modelrouting.ProtocolAnthropicMessages)
	var capturedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: message_start\n"))
		_, _ = w.Write([]byte(`data: {"type":"message_start","message":{"id":"msg-1","type":"message","role":"assistant","model":"fixture-messages","content":[],"stop_reason":"end_turn","usage":{"output_tokens":5,"input_tokens":5}}}` + "\n\n"))
		_, _ = w.Write([]byte("event: content_block_start\n"))
		_, _ = w.Write([]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}` + "\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\n"))
		_, _ = w.Write([]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n"))
		_, _ = w.Write([]byte("event: content_block_stop\n"))
		_, _ = w.Write([]byte(`data: {"type":"content_block_stop","index":0}` + "\n\n"))
		_, _ = w.Write([]byte("event: message_delta\n"))
		_, _ = w.Write([]byte(`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5,"input_tokens":5}}` + "\n\n"))
		_, _ = w.Write([]byte("event: message_stop\n"))
		_, _ = w.Write([]byte(`data: {"type":"message_stop"}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	exec := NewOpenCodeExecutor(constant.OpenCode)
	exec.cfg = &config.Config{}

	auth := &clipoauth.Auth{
		Provider:   constant.OpenCode,
		Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL},
	}
	req := cliproxyexecutor.Request{
		Model:   "fixture-messages",
		Payload: []byte(`{"model":"fixture-messages","messages":[{"role":"user","content":"hi"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI}

	resp, err := exec.Execute(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if capturedPath != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", capturedPath)
	}
	if len(resp.Payload) == 0 {
		t.Error("expected non-empty response payload")
	}
}

func TestOpenCodeExecutor_ExecuteUnknownModelReturnsError(t *testing.T) {
	exec := NewOpenCodeExecutor(constant.OpenCode)
	exec.cfg = &config.Config{}

	auth := &clipoauth.Auth{Provider: constant.OpenCode, Metadata: map[string]any{"api_key": "k"}}
	req := cliproxyexecutor.Request{Model: "nonexistent-model-x9"}

	_, err := exec.Execute(context.Background(), auth, req, cliproxyexecutor.Options{})
	if err == nil {
		t.Fatal("expected error for unknown model, got nil")
	}
	if !strings.Contains(err.Error(), "not available") {
		t.Errorf("error = %v, want 'not available'", err)
	}
}

func TestOpenCodeExecutor_CloneAuthWithBaseURL(t *testing.T) {
	exec := NewOpenCodeExecutor(constant.OpenCode)
	auth := &clipoauth.Auth{
		Provider:   constant.OpenCode,
		Attributes: map[string]string{},
		Metadata:   map[string]any{"api_key": "token"},
	}
	cloned := exec.cloneAuthWithBaseURL(auth)
	if cloned == nil {
		t.Fatal("expected non-nil cloned auth")
	}
	if cloned.Attributes["base_url"] != OpenCodeZenBaseURL {
		t.Errorf("base_url = %q, want %q", cloned.Attributes["base_url"], OpenCodeZenBaseURL)
	}
	if len(auth.Attributes) != 0 {
		t.Error("original auth Attributes was mutated")
	}
}

func TestOpenCodeCredentialsFromMetadata(t *testing.T) {
	auth := &clipoauth.Auth{Metadata: map[string]any{"api_key": "sk-test"}}
	token, ok := openCodeCreds(auth)
	if !ok || token != "sk-test" {
		t.Fatalf("openCodeCreds = (%q, %v), want (sk-test, true)", token, ok)
	}
}

func TestOpenCodeExecutor_NilAuthClone(t *testing.T) {
	exec := NewOpenCodeExecutor(constant.OpenCode)
	cloned := exec.cloneAuthWithBaseURL(nil)
	if cloned != nil {
		t.Fatalf("expected nil, got %v", cloned)
	}
}

// openCodeSessionServer answers both the chat and messages paths and records
// the X-Opencode-Session header each upstream request carried.
func openCodeSessionServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	seen := &[]string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Header.Get("X-Opencode-Session"))
		if r.URL.Path == "/v1/messages" {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			for _, frame := range []string{
				"event: message_start\n" + `data: {"type":"message_start","message":{"id":"msg-1","type":"message","role":"assistant","model":"fixture-messages","content":[],"stop_reason":"end_turn","usage":{"output_tokens":5,"input_tokens":5}}}`,
				"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}`,
				"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
				"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}`,
				"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5,"input_tokens":5}}`,
				"event: message_stop\n" + `data: {"type":"message_stop"}`,
				"data: [DONE]",
			} {
				_, _ = w.Write([]byte(frame + "\n\n"))
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"c-1","object":"chat.completion","choices":[{"index":0,"message":{"content":"OK","role":"assistant"}}]}`))
	}))
	t.Cleanup(server.Close)
	return server, seen
}

func TestOpenCodeExecutor_SendsConversationSession(t *testing.T) {
	registerOpenCodeTestRoute(t, constant.OpenCodeGo, "fixture-chat", modelrouting.ProtocolOpenAIChat)
	registerOpenCodeTestRoute(t, constant.OpenCodeGo, "fixture-messages", modelrouting.ProtocolAnthropicMessages)
	derived := map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:probe"}
	send := func(t *testing.T, model string, metadata map[string]any, headers http.Header) string {
		t.Helper()
		server, seen := openCodeSessionServer(t)
		exec := NewOpenCodeExecutor(constant.OpenCodeGo)
		exec.cfg = &config.Config{}
		auth := &clipoauth.Auth{
			Provider:   constant.OpenCodeGo,
			Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL},
		}
		payload := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"Respond with OK."}]}`)
		req := cliproxyexecutor.Request{Model: model, Payload: payload, Metadata: metadata}
		opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, Headers: headers, OriginalRequest: payload, Metadata: metadata}
		if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
			t.Fatalf("Execute error: %v", err)
		}
		if len(*seen) != 1 {
			t.Fatalf("upstream requests = %d, want 1", len(*seen))
		}
		if _, leaked := auth.Attributes["header:X-Opencode-Session"]; leaked {
			t.Fatal("request session leaked into the shared auth attributes")
		}
		if _, err := uuid.Parse((*seen)[0]); err != nil {
			t.Fatalf("upstream X-Opencode-Session = %q, want a UUID: %v", (*seen)[0], err)
		}
		return (*seen)[0]
	}

	chat := send(t, "fixture-chat", derived, nil)
	if messages := send(t, "fixture-messages", derived, nil); messages != chat {
		t.Fatalf("one conversation sent two sessions: chat %q, messages %q", chat, messages)
	}
	if other := send(t, "fixture-chat", map[string]any{cliproxyexecutor.DerivedSessionIDMetadataKey: "ctx:v1:other"}, nil); other == chat {
		t.Fatalf("two conversations shared session %q", chat)
	}
	// A request with no declared or derived identity still carries a session:
	// the gateway rejects one without it.
	send(t, "fixture-chat", nil, nil)

	// A session the client declared selects the upstream session, whatever
	// header it used and whatever characters it holds, and never leaves verbatim.
	for _, headers := range []http.Header{
		{"X-Opencode-Session": {"client-session-1"}},
		{"X-Session-Id": {"agent-session-2"}},
		{"X-Opencode-Session": {"$X-Session-Id"}},
	} {
		declared := send(t, "fixture-chat", derived, headers)
		if declared == chat {
			t.Fatalf("declared session %v did not select the upstream session", headers)
		}
		if again := send(t, "fixture-chat", nil, headers); again != declared {
			t.Fatalf("declared session %v is not stable: %q then %q", headers, declared, again)
		}
	}
}

// An OpenCode credential may keep its key in metadata; the delegated messages
// executor must still authenticate with it instead of calling anonymously.
func TestOpenCodeExecutor_DelegatedMessagesSendMetadataKey(t *testing.T) {
	registerOpenCodeTestRoute(t, constant.OpenCode, "fixture-messages", modelrouting.ProtocolAnthropicMessages)
	var gotAuth, gotXAPIKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotXAPIKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for _, frame := range []string{
			"event: message_start\n" + `data: {"type":"message_start","message":{"id":"msg-1","type":"message","role":"assistant","model":"fixture-messages","content":[],"stop_reason":"end_turn","usage":{"output_tokens":1,"input_tokens":1}}}`,
			"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"OK"}}`,
			"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":0}`,
			"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1,"input_tokens":1}}`,
			"event: message_stop\n" + `data: {"type":"message_stop"}`,
		} {
			_, _ = w.Write([]byte(frame + "\n\n"))
		}
	}))
	t.Cleanup(server.Close)

	exec := NewOpenCodeExecutor(constant.OpenCode)
	exec.cfg = &config.Config{}
	auth := &clipoauth.Auth{
		Provider:   constant.OpenCode,
		Attributes: map[string]string{"base_url": server.URL},
		Metadata:   map[string]any{"api_key": "metadata-key"},
	}
	payload := []byte(`{"model":"fixture-messages","messages":[{"role":"user","content":"Respond with OK."}]}`)
	req := cliproxyexecutor.Request{Model: "fixture-messages", Payload: payload}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: payload}
	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotXAPIKey != "metadata-key" || gotAuth != "" {
		t.Fatalf("delegated request authentication = (Authorization %q, X-Api-Key %q), want x-api-key with the metadata key", gotAuth, gotXAPIKey)
	}
	if _, leaked := auth.Attributes["api_key"]; leaked {
		t.Fatal("request key leaked into the shared auth attributes")
	}
}

func TestOpenCodeExecutor_PreservesNativeResponsesSSEFrames(t *testing.T) {
	registerOpenCodeTestRoute(t, constant.OpenCodeGo, "fixture-responses", modelrouting.ProtocolOpenAIResponses)
	const events = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"PONG\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("upstream path = %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(events))
	}))
	defer server.Close()
	exec := NewOpenCodeExecutor(constant.OpenCodeGo)
	exec.cfg = &config.Config{}
	auth := &clipoauth.Auth{
		Provider:   constant.OpenCodeGo,
		Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL},
	}
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model: "fixture-responses", Payload: []byte(`{"model":"fixture-responses","input":"Reply with PONG.","stream":true}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	if err != nil {
		t.Fatal(err)
	}
	var received strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatal(chunk.Err)
		}
		received.Write(chunk.Payload)
	}
	if received.String() != events {
		t.Fatalf("native Responses SSE framing changed: %q", received.String())
	}
}

func TestOpenCodeCatalogUsesDeclaredCanonicalIDAndAPICapability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" || r.Header.Get("Authorization") != "" {
			t.Errorf("unexpected public catalog request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[
		{"id":"model-from-live-catalog-7f3a","canonical_id":"owner/exact-release","type":"chat","api":{"responses":{}},"cost":{"text":{"input":{"price":0},"output":{"price":0}}}},
		{"id":"unroutable-live-model","canonical_id":"owner/mismatched","api":{"unsupported":{}}}
		]}`))
	}))
	defer server.Close()
	auth := &clipoauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "private"}}
	models, err := FetchOpenCodeModels(context.Background(), auth, &config.Config{}, "opencode-go")
	if err != nil || len(models) != 1 {
		t.Fatalf("expected one executable model, got %v, %v", models, err)
	}
	model := models[0]
	if model.ID != "model-from-live-catalog-7f3a" || model.CatalogProviderID != "owner" ||
		model.CatalogModelID != "exact-release" || model.CatalogRouteProviderID != "opencode-go" ||
		model.CatalogRouteModelID != model.ID {
		t.Fatalf("model identity does not preserve the producer's declaration: %+v", model)
	}
	if !model.FreeTextQuota || len(model.Protocols) != 1 || model.Protocols[0] != modelrouting.ProtocolOpenAIResponses {
		t.Fatalf("live API and free price were not propagated: %+v", model)
	}
	clientID := "test-opencode-live-catalog-" + uuid.NewString()
	registry.GetGlobalRegistry().RegisterClient(clientID, constant.OpenCodeGo, models)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(clientID) })
	exec := NewOpenCodeExecutor(constant.OpenCodeGo)
	protocol, path, err := exec.resolveRoute(model.ID)
	if err != nil || protocol != modelrouting.ProtocolOpenAIResponses || path != "/v1/responses" {
		t.Fatalf("new catalog model route = (%q, %q, %v)", protocol, path, err)
	}
}

func TestOpenCodeCatalogIncludesNewIDsFromDeclaredAPIs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"never-seen-chat-id","api":{"chat-completions":{}}},
			{"id":"never-seen-messages-id","api":{"messages":{}}},
			{"id":"never-seen-responses-id","api":{"responses":{}}},
			{"id":"unsupported-id","api":{"generate-content":{}}}
		]}`))
	}))
	defer server.Close()
	auth := &clipoauth.Auth{Attributes: map[string]string{"base_url": server.URL}}
	models, err := FetchOpenCodeModels(context.Background(), auth, &config.Config{}, constant.OpenCodeGo)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 3 {
		t.Fatalf("models = %d, want 3 declared executable APIs", len(models))
	}
	want := map[string]string{
		"never-seen-chat-id":      modelrouting.ProtocolOpenAIChat,
		"never-seen-messages-id":  modelrouting.ProtocolAnthropicMessages,
		"never-seen-responses-id": modelrouting.ProtocolOpenAIResponses,
	}
	for _, model := range models {
		if len(model.Protocols) != 1 || model.Protocols[0] != want[model.ID] {
			t.Errorf("model %q protocols = %v, want [%s]", model.ID, model.Protocols, want[model.ID])
		}
	}
}

func TestOpenCodeGatewaysResolveSameIDFromTheirOwnCatalogDeclaration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var api string
		switch r.URL.Path {
		case "/zen/v1/models":
			api = `"responses":{}`
		case "/go/v1/models":
			api = `"chat-completions":{}`
		default:
			t.Errorf("unexpected catalog path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprintf(w, `{"data":[{"id":"newly-announced-model","api":{%s}}]}`, api)
	}))
	defer server.Close()
	for _, tc := range []struct {
		provider string
		gateway  string
		apiPath  string
		protocol string
	}{
		{constant.OpenCode, "/zen", "/v1/responses", modelrouting.ProtocolOpenAIResponses},
		{constant.OpenCodeGo, "/go", "/v1/chat/completions", modelrouting.ProtocolOpenAIChat},
	} {
		auth := &clipoauth.Auth{Attributes: map[string]string{"base_url": server.URL + tc.gateway}}
		models, err := FetchOpenCodeModels(context.Background(), auth, &config.Config{}, tc.provider)
		if err != nil || len(models) != 1 {
			t.Fatalf("FetchOpenCodeModels(%s) = %d models, %v", tc.provider, len(models), err)
		}
		clientID := "test-opencode-gateway-" + uuid.NewString()
		registry.GetGlobalRegistry().RegisterClient(clientID, tc.provider, models)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(clientID) })
		protocol, path, err := NewOpenCodeExecutor(tc.provider).resolveRoute("newly-announced-model")
		if err != nil || protocol != tc.protocol || path != tc.apiPath {
			t.Fatalf("%s live declaration resolved to (%q, %q, %v)", tc.provider, protocol, path, err)
		}
	}
}

// Unary and stream failures preserve the reset time and the billing pool.
func TestOpenCodeExecutor_UsageLimitKeepsRetryAfterAndScope(t *testing.T) {
	for _, tc := range []struct {
		provider  string
		model     string
		errorType string
		poolCode  string
	}{
		{constant.OpenCodeGo, "fixture-subscription-chat", "GoUsageLimitError", clipoauth.ErrorCodeOpenCodeSubscriptionLimit},
		{constant.OpenCodeGo, "fixture-subscription-messages", "GoUsageLimitError", clipoauth.ErrorCodeOpenCodeSubscriptionLimit},
		{constant.OpenCodeGo, "fixture-subscription-responses", "GoUsageLimitError", clipoauth.ErrorCodeOpenCodeSubscriptionLimit},
		{constant.OpenCodeGo, "fixture-free-chat", "FreeUsageLimitError", clipoauth.ErrorCodeOpenCodeFreeLimit},
		{constant.OpenCode, "fixture-free-messages", "FreeUsageLimitError", clipoauth.ErrorCodeOpenCodeFreeLimit},
	} {
		t.Run(tc.model, func(t *testing.T) {
			protocol := modelrouting.ProtocolOpenAIChat
			if strings.Contains(tc.model, "messages") {
				protocol = modelrouting.ProtocolAnthropicMessages
			}
			if strings.Contains(tc.model, "responses") {
				protocol = modelrouting.ProtocolOpenAIResponses
			}
			registerOpenCodeTestRoute(t, tc.provider, tc.model, protocol)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "273866")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"` + tc.errorType + `","message":"usage limit reached"}}`))
			}))
			t.Cleanup(server.Close)
			exec := NewOpenCodeExecutor(tc.provider)
			exec.cfg = &config.Config{}
			auth := &clipoauth.Auth{
				Provider:   tc.provider,
				Attributes: map[string]string{"api_key": "test-key", "base_url": server.URL},
			}
			payload := []byte(`{"model":"` + tc.model + `","messages":[{"role":"user","content":"PONG"}]}`)
			req := cliproxyexecutor.Request{Model: tc.model, Payload: payload}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatOpenAI, OriginalRequest: payload}
			want := 273866 * time.Second
			check := func(path string, err error) {
				t.Helper()
				retry, ok := err.(interface{ RetryAfter() *time.Duration })
				if !ok || retry.RetryAfter() == nil || *retry.RetryAfter() != want {
					t.Fatalf("%s: error %v does not carry Retry-After %s", path, err, want)
				}
				scoped, ok := err.(interface{ IsCredentialScoped() bool })
				if !ok || scoped.IsCredentialScoped() || clipoauth.ResultErrorFromError(err).Code != tc.poolCode {
					t.Fatalf("%s: expected a pool-scoped error %q, got %+v", path, tc.poolCode, clipoauth.ResultErrorFromError(err))
				}
			}
			_, err := exec.Execute(context.Background(), auth, req, opts)
			check("execute", err)
			_, err = exec.ExecuteStream(context.Background(), auth, req, opts)
			check("stream", err)
		})
	}
}
