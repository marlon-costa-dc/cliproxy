package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/modelrouting"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	clipoauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

const (
	OpenCodeZenBaseURL = "https://opencode.ai/zen"
	OpenCodeGoBaseURL  = "https://opencode.ai/zen/go"
)

type OpenCodeExecutor struct {
	id      string
	gateway string
	cfg     *config.Config
}

func NewOpenCodeExecutor(id string) *OpenCodeExecutor {
	gateway := "zen"
	if id == constant.OpenCodeGo || id == "opencode-go" {
		gateway = "go"
	}
	return &OpenCodeExecutor{id: id, gateway: gateway}
}

// Match exact JSON zero, including scientific notation, without rounding a tiny price to zero.
var zeroCatalogPrice = regexp.MustCompile(`^-?0(\.0+)?([eE][+-]?[0-9]+)?$`)

type openCodeTextRate struct {
	Price     json.Number                `json:"price"`
	Cache     map[string]json.RawMessage `json:"cache"`
	Overrides []json.RawMessage          `json:"overrides"`
}

func (rate openCodeTextRate) free() bool {
	// Conditional and tiered rates require separate qualification.
	if !zeroCatalogPrice.MatchString(rate.Price.String()) || len(rate.Overrides) > 0 {
		return false
	}
	for _, price := range rate.Cache {
		if !zeroCatalogPrice.Match(price) {
			return false
		}
	}
	return true
}

// FetchOpenCodeModels joins the gateway's explicit model identity to routes
// whose wire protocol this executor supports. The public catalog does not
// prove account availability; each credential still requires inference.
func FetchOpenCodeModels(ctx context.Context, auth *clipoauth.Auth, cfg *config.Config, provider string) ([]*registry.ModelInfo, error) {
	e := NewOpenCodeExecutor(provider)
	routeAuth := e.cloneAuthWithBaseURL(auth)
	if routeAuth == nil {
		return nil, fmt.Errorf("OpenCode catalog requires route auth")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(routeAuth.Attributes["base_url"], "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := newProxyAwareHTTPClient(ctx, cfg, routeAuth, 10*time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenCode catalog returned HTTP %d", resp.StatusCode)
	}
	const maxCatalogBytes = 8 << 20
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxCatalogBytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > maxCatalogBytes {
		return nil, fmt.Errorf("OpenCode catalog exceeds %d bytes", maxCatalogBytes)
	}
	var catalog struct {
		Data []struct {
			ID          string `json:"id"`
			CanonicalID string `json:"canonical_id"`
			Cost        struct {
				Text struct {
					Input  openCodeTextRate `json:"input"`
					Output openCodeTextRate `json:"output"`
				} `json:"text"`
			} `json:"cost"`
			API map[string]json.RawMessage `json:"api"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &catalog); err != nil {
		return nil, fmt.Errorf("decode OpenCode catalog: %w", err)
	}
	if len(catalog.Data) == 0 {
		return nil, fmt.Errorf("OpenCode catalog contains no models")
	}
	seenIDs := make(map[string]struct{}, len(catalog.Data))
	for _, row := range catalog.Data {
		if _, exists := seenIDs[row.ID]; exists || row.ID == "" {
			return nil, fmt.Errorf("OpenCode catalog has missing or duplicate model identity")
		}
		seenIDs[row.ID] = struct{}{}
	}
	available := make([]*registry.ModelInfo, 0, len(catalog.Data))
	for _, row := range catalog.Data {
		protocol := openCodeDeclaredProtocol(row.API)
		if protocol == "" {
			continue
		}
		model := &registry.ModelInfo{
			ID:          row.ID,
			Object:      "model",
			OwnedBy:     "opencode",
			Type:        "opencode",
			DisplayName: row.ID,
			Description: "OpenCode " + e.gateway + " gateway — " + row.ID,
			Protocols:   []string{protocol},
			FreeTextQuota: row.Cost.Text.Input.free() &&
				row.Cost.Text.Output.free(),
		}
		if row.CanonicalID != "" {
			owner, canonical, ok := strings.Cut(row.CanonicalID, "/")
			if !ok || owner == "" || canonical == "" || strings.TrimSpace(row.CanonicalID) != row.CanonicalID {
				return nil, fmt.Errorf("OpenCode catalog has invalid canonical identity for %q", model.ID)
			}
			model.CatalogProviderID = owner
			model.CatalogModelID = canonical
			model.CatalogRouteProviderID = provider
			model.CatalogRouteModelID = model.ID
		}
		available = append(available, model)
	}
	return available, nil
}

// openCodeDeclaredProtocol derives the executable wire protocol solely from
// the model's declared gateway API capabilities. Unsupported APIs are not
// advertised as direct text routes.
func openCodeDeclaredProtocol(apis map[string]json.RawMessage) string {
	for _, declared := range []struct{ api, protocol string }{
		{"chat-completions", modelrouting.ProtocolOpenAIChat},
		{"responses", modelrouting.ProtocolOpenAIResponses},
		{"messages", modelrouting.ProtocolAnthropicMessages},
	} {
		if _, ok := apis[declared.api]; ok {
			return declared.protocol
		}
	}
	return ""
}

func (e *OpenCodeExecutor) Identifier() string  { return e.id }
func (e *OpenCodeExecutor) ProviderKey() string { return e.id }

func (e *OpenCodeExecutor) RequestToFormat(req cliproxyexecutor.Request, _ cliproxyexecutor.Options) sdktranslator.Format {
	model := registry.GetGlobalRegistry().GetModelInfo(thinking.ParseSuffix(req.Model).ModelName, e.id)
	if model == nil {
		return sdktranslator.FormatOpenAI
	}
	_, format := openCodeRoute(firstOpenCodeProtocol(model.Protocols))
	return format
}

func firstOpenCodeProtocol(protocols []string) string {
	if len(protocols) == 0 {
		return ""
	}
	return protocols[0]
}

func (e *OpenCodeExecutor) cloneAuthWithBaseURL(auth *clipoauth.Auth) *clipoauth.Auth {
	if auth == nil {
		return nil
	}
	cloned := *auth
	cloned.Attributes = make(map[string]string, len(auth.Attributes)+2)
	for k, v := range auth.Attributes {
		cloned.Attributes[k] = v
	}
	if strings.TrimSpace(cloned.Attributes["base_url"]) == "" {
		if auth.Metadata != nil {
			if v, ok := auth.Metadata["base_url"].(string); ok && strings.TrimSpace(v) != "" {
				cloned.Attributes["base_url"] = strings.TrimSpace(v)
			}
		}
	}
	if strings.TrimSpace(cloned.Attributes["base_url"]) == "" {
		switch e.gateway {
		case "go":
			cloned.Attributes["base_url"] = OpenCodeGoBaseURL
		default:
			cloned.Attributes["base_url"] = OpenCodeZenBaseURL
		}
	}
	return &cloned
}

// openCodeSessionHeader is the conversation identity OpenCode gateways route
// and cache by; OpenCode Go rejects a request without it (MissingSessionID).
const openCodeSessionHeader = "X-Opencode-Session"

// requestAuth clones auth for one request and carries the conversation's
// OpenCode session as a header attribute, so every protocol path - including
// the delegated Claude and Gemini executors - sends it upstream. The session is
// the identity credential affinity already selected by, mapped to a
// provider-scoped UUID so no client-supplied identifier leaves verbatim.
func (e *OpenCodeExecutor) requestAuth(auth *clipoauth.Auth, opts cliproxyexecutor.Options) *clipoauth.Auth {
	cloned := e.cloneAuthWithBaseURL(auth)
	if cloned == nil {
		return nil
	}
	// The delegated Claude and Gemini executors read the key only from
	// attributes; an OpenCode credential may carry it in metadata.
	if apiKey, ok := openCodeCreds(auth); ok {
		cloned.Attributes["api_key"] = apiKey
	}
	sessionID := helps.AffinitySessionUUID(e.id, clipoauth.ExtractSessionID(opts.Headers, opts.OriginalRequest, opts.Metadata))
	if sessionID != "" {
		cloned.Attributes["header:"+openCodeSessionHeader] = sessionID
	}
	return cloned
}

// openCodeSetBearer authenticates requests on the OpenAI-compatible transport.
func openCodeSetBearer(req *http.Request, apiKey string) {
	if req == nil || apiKey == "" {
		return
	}
	req.Header.Del("x-api-key")
	req.Header.Set("Authorization", "Bearer "+apiKey)
}

func (e *OpenCodeExecutor) resolveRoute(modelID string) (protocol string, url string, err error) {
	model := registry.GetGlobalRegistry().GetModelInfo(modelID, e.id)
	if model == nil || len(model.Protocols) != 1 {
		return "", "", fmt.Errorf("opencode: model %q is not available on the %s gateway", modelID, e.gateway)
	}
	protocol = firstOpenCodeProtocol(model.Protocols)
	url, _ = openCodeRoute(protocol)
	if url == "" {
		return "", "", fmt.Errorf("opencode: model %q declares unsupported protocol %q", modelID, protocol)
	}
	return protocol, url, nil
}

func openCodeRoute(protocol string) (string, sdktranslator.Format) {
	switch protocol {
	case modelrouting.ProtocolOpenAIResponses:
		return "/v1/responses", sdktranslator.FormatOpenAIResponse
	case modelrouting.ProtocolAnthropicMessages:
		return "/v1/messages", sdktranslator.FormatClaude
	case modelrouting.ProtocolOpenAIChat:
		return "/v1/chat/completions", sdktranslator.FormatOpenAI
	}
	return "", sdktranslator.FormatOpenAI
}

func (e *OpenCodeExecutor) Execute(ctx context.Context, auth *clipoauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	protocol, path, errRoute := e.resolveRoute(baseModel)
	if errRoute != nil {
		return cliproxyexecutor.Response{}, errRoute
	}
	clonedAuth := e.requestAuth(auth, opts)
	baseURL := strings.TrimSuffix(clonedAuth.Attributes["base_url"], "/")

	log.Debugf("opencode executor [%s]: model=%q protocol=%q path=%s", e.gateway, baseModel, protocol, path)

	switch protocol {
	case modelrouting.ProtocolAnthropicMessages:
		// The legacy native Messages endpoint accepts the Anthropic API key header.
		clonedAuth.Attributes["anthropic_auth_scheme"] = "x-api-key"
		ce := NewClaudeExecutor(e.cfg)
		ce.gatewayQuota = openCodeGatewayQuota
		return ce.Execute(ctx, clonedAuth, req, opts)
	case modelrouting.ProtocolOpenAIResponses:
		fullURL := baseURL + path
		return e.executeOpenAIEndpoint(ctx, clonedAuth, req, opts, fullURL, sdktranslator.FormatOpenAIResponse)
	case modelrouting.ProtocolOpenAIChat:
		fullURL := baseURL + path
		return e.executeOpenAIEndpoint(ctx, clonedAuth, req, opts, fullURL, sdktranslator.FormatOpenAI)
	}
	return cliproxyexecutor.Response{}, fmt.Errorf("opencode: unsupported protocol %q for model %q", protocol, baseModel)
}

func (e *OpenCodeExecutor) ExecuteStream(ctx context.Context, auth *clipoauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	protocol, path, errRoute := e.resolveRoute(baseModel)
	if errRoute != nil {
		return nil, errRoute
	}
	clonedAuth := e.requestAuth(auth, opts)
	baseURL := strings.TrimSuffix(clonedAuth.Attributes["base_url"], "/")

	log.Debugf("opencode executor [%s]: stream model=%q protocol=%q path=%s", e.gateway, baseModel, protocol, path)

	switch protocol {
	case modelrouting.ProtocolAnthropicMessages:
		// The legacy native Messages endpoint accepts the Anthropic API key header.
		clonedAuth.Attributes["anthropic_auth_scheme"] = "x-api-key"
		ce := NewClaudeExecutor(e.cfg)
		ce.gatewayQuota = openCodeGatewayQuota
		return ce.ExecuteStream(ctx, clonedAuth, req, opts)
	case modelrouting.ProtocolOpenAIResponses:
		fullURL := baseURL + path
		return e.executeOpenAIEndpointStream(ctx, clonedAuth, req, opts, fullURL, sdktranslator.FormatOpenAIResponse)
	case modelrouting.ProtocolOpenAIChat:
		fullURL := baseURL + path
		return e.executeOpenAIEndpointStream(ctx, clonedAuth, req, opts, fullURL, sdktranslator.FormatOpenAI)
	}
	return nil, fmt.Errorf("opencode: unsupported protocol %q for model %q", protocol, baseModel)
}

func (e *OpenCodeExecutor) executeOpenAIEndpoint(ctx context.Context, auth *clipoauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointURL string, toFormat sdktranslator.Format) (resp cliproxyexecutor.Response, err error) {
	reporter := helps.NewExecutorUsageReporter(ctx, e, thinking.ParseSuffix(req.Model).ModelName, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	upstreamModel := thinking.ParseSuffix(req.Model).ModelName

	body := req.Payload
	if len(body) == 0 && len(opts.OriginalRequest) > 0 {
		body = opts.OriginalRequest
	}
	translated := helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, toFormat, upstreamModel, body, false, false)
	translated, err = helps.ApplyRequestThinking(translated, req, opts, from.String(), toFormat.String(), e.id)
	if err != nil {
		return resp, fmt.Errorf("opencode executor: apply thinking: %w", err)
	}
	translated, err = sjson.SetBytes(translated, "model", upstreamModel)
	if err != nil {
		return resp, fmt.Errorf("opencode executor: set model: %w", err)
	}

	apiKey, _ := openCodeCreds(auth)
	if apiKey == "" {
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(translated))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	openCodeSetBearer(httpReq, apiKey)
	httpReq.Header.Set("User-Agent", "cli-proxy-opencode")
	util.ApplyCustomHeadersFromAttrs(httpReq, auth.Attributes, opts.Headers)

	authID, authLabel, authType, authValue := authAccountInfo(auth)
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       endpointURL,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      translated,
		Provider:  e.id,
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("opencode executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	data, errRead := io.ReadAll(httpResp.Body)
	if errRead != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errRead)
		return resp, errRead
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		helps.LogWithRequestID(ctx).Debugf("opencode executor: error status: %d, message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), data))
		err = newOpenCodeStatusError(httpResp, data)
		return resp, err
	}

	reporter.Publish(ctx, helps.ParseOpenAIUsage(data))
	reporter.EnsurePublished(ctx)
	resp = cliproxyexecutor.Response{Payload: data, Headers: httpResp.Header.Clone()}
	return resp, nil
}

func (e *OpenCodeExecutor) executeOpenAIEndpointStream(ctx context.Context, auth *clipoauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, endpointURL string, toFormat sdktranslator.Format) (*cliproxyexecutor.StreamResult, error) {
	var err error
	reporter := helps.NewExecutorUsageReporter(ctx, e, thinking.ParseSuffix(req.Model).ModelName, auth)
	defer reporter.TrackFailure(ctx, &err)

	from := opts.SourceFormat
	upstreamModel := thinking.ParseSuffix(req.Model).ModelName

	body := req.Payload
	if len(body) == 0 && len(opts.OriginalRequest) > 0 {
		body = opts.OriginalRequest
	}
	translated := helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, toFormat, upstreamModel, body, true, false)
	translated, err = sjson.SetBytes(translated, "stream", true)
	if err != nil {
		return nil, fmt.Errorf("opencode executor: set stream: %w", err)
	}

	apiKey, _ := openCodeCreds(auth)
	if apiKey == "" {
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(translated))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	openCodeSetBearer(httpReq, apiKey)
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	httpReq.Header.Set("User-Agent", "cli-proxy-opencode")
	util.ApplyCustomHeadersFromAttrs(httpReq, auth.Attributes, opts.Headers)

	authID, authLabel, authType, authValue := authAccountInfo(auth)
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       endpointURL,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      translated,
		Provider:  e.id,
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("opencode executor: stream error status: %d, message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("opencode executor: close response body error: %v", errClose)
		}
		return nil, newOpenCodeStatusError(httpResp, b)
	}

	out := make(chan cliproxyexecutor.StreamChunk, 64)
	go func() {
		defer close(out)
		defer reporter.EnsurePublished(ctx)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("opencode executor: close stream body error: %v", errClose)
			}
		}()

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800)
		var param any
		var streamUsage helps.StreamUsageBuffer
		defer streamUsage.Publish(ctx, reporter)

		for scanner.Scan() {
			line := scanner.Bytes()
			helps.AppendAPIResponseChunk(ctx, e.cfg, line)
			streamUsage.ObserveOpenAIStream(line)
			// Native Responses carries event and data lines in the same SSE
			// frame. Preserve their separators across the line scanner.
			frameLine := bytes.Clone(line)
			if sourceFormatEqual(from, toFormat) && toFormat == sdktranslator.FormatOpenAIResponse {
				frameLine = append(frameLine, '\n')
			}
			chunks := sdktranslator.TranslateStream(
				ctx,
				from,
				toFormat,
				req.Model,
				opts.OriginalRequest,
				translated,
				frameLine,
				&param,
			)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return
				}
			}
		}
		if errScan := scanner.Err(); errScan != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errScan)
			reporter.PublishFailure(ctx, errScan)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
			case <-ctx.Done():
			}
		}
	}()

	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *OpenCodeExecutor) PrepareRequest(req *http.Request, auth *clipoauth.Auth) error {
	if req == nil {
		return nil
	}
	token, _ := openCodeCreds(auth)
	openCodeSetBearer(req, token)
	return nil
}

func (e *OpenCodeExecutor) HttpRequest(ctx context.Context, auth *clipoauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, statusErr{code: http.StatusInternalServerError, msg: "opencode executor: request is nil"}
	}
	if ctx == nil {
		ctx = req.Context()
	}
	if err := e.PrepareRequest(req, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(req.WithContext(ctx))
}

func (e *OpenCodeExecutor) Refresh(_ context.Context, auth *clipoauth.Auth) (*clipoauth.Auth, error) {
	return auth, nil
}

func (e *OpenCodeExecutor) CountTokens(ctx context.Context, auth *clipoauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	payload := []byte("{}")
	if len(req.Payload) > 0 {
		payload = req.Payload
	}
	count := int64(len(payload))
	return cliproxyexecutor.Response{Payload: []byte(fmt.Sprintf(`{"input_tokens":%d,"output_tokens":0,"total_tokens":%d}`, count, count))}, nil
}

func openCodeCreds(auth *clipoauth.Auth) (string, bool) {
	if auth == nil {
		return "", false
	}
	if token := strings.TrimSpace(auth.Attributes["api_key"]); token != "" {
		return token, true
	}
	if auth.Metadata == nil {
		return "", false
	}
	if v, ok := auth.Metadata["api_key"]; ok {
		if token, ok := v.(string); ok && strings.TrimSpace(token) != "" {
			return token, true
		}
	}
	if v, ok := auth.Metadata["access_token"]; ok {
		if token, ok := v.(string); ok && strings.TrimSpace(token) != "" {
			return token, true
		}
	}
	return "", false
}

func authAccountInfo(auth *clipoauth.Auth) (string, string, string, string) {
	if auth == nil {
		return "", "", "", ""
	}
	authType, authValue := auth.AccountInfo()
	return auth.ID, auth.Label, authType, authValue
}

// An OpenCode usage limit belongs to its billing pool on the credential.
type openCodeUsageLimit struct {
	statusErr
	pool string
}

func (e openCodeUsageLimit) ErrorCode() string { return e.pool }

// A delegate keeps its native error semantics unless its gateway recognizes
// the response as a provider quota limit.
type gatewayQuotaClassifier func(*http.Response, []byte) error

func (classify gatewayQuotaClassifier) limit(resp *http.Response, body []byte) error {
	if classify == nil || resp.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	return classify(resp, body)
}

func openCodeGatewayQuota(resp *http.Response, body []byte) error {
	var pool string
	switch ojson.GetBytesNoCopy(body, "error.type").String() {
	case "GoUsageLimitError":
		pool = clipoauth.ErrorCodeOpenCodeSubscriptionLimit
	case "FreeUsageLimitError":
		pool = clipoauth.ErrorCodeOpenCodeFreeLimit
	default:
		return nil
	}
	return openCodeUsageLimit{newOpenAICompatStatusError(resp.StatusCode, resp.Header, body), pool}
}

func newOpenCodeStatusError(resp *http.Response, body []byte) error {
	if limit := gatewayQuotaClassifier(openCodeGatewayQuota).limit(resp, body); limit != nil {
		return limit
	}
	return newOpenAICompatStatusError(resp.StatusCode, resp.Header, body)
}
