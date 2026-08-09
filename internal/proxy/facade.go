package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hellowind777/hellogrok/internal/appinfo"
	"github.com/hellowind777/hellogrok/internal/config"
	"github.com/hellowind777/hellogrok/internal/patch"
)

const maxFacadeBodyBytes int64 = 64 << 20

var errBodyTooLarge = errors.New("body exceeds size limit")

func (s *Server) forwardFacade(w http.ResponseWriter, incoming *http.Request, route config.Route, incomingProtocol wireProtocol) {
	defer s.flushReasoningProvenance()
	if incoming.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "custom channel facade accepts POST only")
		return
	}
	if !isJSONContentType(incoming.Header.Get("Content-Type")) {
		writeJSONError(w, http.StatusUnsupportedMediaType, "custom channel facade requires application/json")
		return
	}
	if incoming.ContentLength > maxFacadeBodyBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request body exceeds 64 MiB")
		return
	}
	body, err := readBodyLimited(incoming.Body, maxFacadeBodyBytes)
	_ = incoming.Body.Close()
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body exceeds 64 MiB")
			return
		}
		writeJSONError(w, http.StatusBadRequest, "read request: "+err.Error())
		return
	}
	request, err := adaptFacadeRequestWithReasoning(body, route, incomingProtocol, s.reasoning, keepUnknownReasoning)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	target, err := upstreamTarget(route, request.Protocol, incoming.URL.RawQuery)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	if !routeHasCredential(route, incoming.Header) && !routeIsLoopback(route) {
		s.log.Printf("UP blocked channel=%s: no channel api_key/env_key; incoming authorization was not forwarded", route.ChannelID)
		writeJSONError(w, http.StatusUnauthorized, "custom channel has no channel-owned credential")
		return
	}

	tools, webSearch, hostedSearch, functionSearch, xSearch := summarizeBody(request.Body)
	logTarget := safeDiagnosticTarget(target)
	s.log.Printf("UP channel=%s incoming=%s upstream=%s kind=%s %s body=%dB model=%s tools=%d web_search=%d hosted_web_search=%d function_web_search=%d x_search=%d build_hosted_web_search=%d build_x_search=%d proxy_added_web_search=%t client_web_search_prepared=%t client_web_search_aliased=%t",
		route.ChannelID, request.IncomingProtocol, request.Protocol, requestKindLabel(request.Kind), logTarget,
		len(request.Body), route.WireModel, tools, webSearch, hostedSearch, functionSearch, xSearch,
		request.BuildHostedWebSearch, request.BuildXSearch, request.ProxyAddedWebSearch,
		request.ClientSearchPrepared, request.ClientSearchAlias != "")
	if request.Reasoning.Opaque > 0 {
		s.log.Printf("UP channel=%s reasoning projection opaque=%d compatible=%d unknown=%d dropped=%d recovery=%t",
			route.ChannelID, request.Reasoning.Opaque, request.Reasoning.Compatible,
			request.Reasoning.Unknown, request.Reasoning.Dropped, request.ReasoningRecovery)
	}
	saveLastRequestMeta(logTarget, route.WireModel, len(request.Body), tools, webSearch, hostedSearch, functionSearch, xSearch, request)
	if incomingModel := extractModel(body); incomingModel != "" && incomingModel != route.WireModel {
		s.log.Printf("UP model isolated channel=%s: %s -> %s", route.ChannelID, incomingModel, route.WireModel)
	}
	if hasIncomingCredential(incoming.Header) {
		if _, ok := incomingProviderCredential(route, incoming.Header); ok {
			s.log.Printf("UP auth isolated channel=%s: used credential from configured channel auth_provider", route.ChannelID)
		} else {
			s.log.Printf("UP auth isolated channel=%s: ignored incoming credential and used channel credential", route.ChannelID)
		}
	}

	started := time.Now()
	upstreamContext, cancelUpstream := context.WithCancel(incoming.Context())
	stopLifecycleCancel := context.AfterFunc(s.upstreamLifecycleContext(), cancelUpstream)
	defer func() {
		stopLifecycleCancel()
		cancelUpstream()
	}()
	doRequest := func(payload []byte) (*http.Response, error) {
		req, err := http.NewRequestWithContext(upstreamContext, http.MethodPost, target, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		u, _ := url.Parse(target)
		req.Host = u.Host
		copySafeRequestHeaders(req.Header, incoming.Header)
		req.Header.Set("Content-Type", "application/json")
		if request.Stream {
			req.Header.Set("Accept", "text/event-stream, application/json")
		} else {
			req.Header.Set("Accept", "application/json")
		}
		if req.Header.Get("User-Agent") == "" {
			req.Header.Set("User-Agent", appinfo.Name+"/"+appinfo.Version)
		}
		applyRouteHeaders(req.Header, route, request.Protocol, incoming.Header)
		if request.Protocol == wireMessages && req.Header.Get("Anthropic-Version") == "" {
			req.Header.Set("Anthropic-Version", "2023-06-01")
		}
		req.ContentLength = int64(len(payload))
		return s.client.Do(req)
	}

	var response *http.Response
	for {
		response, err = doRequest(request.Body)
		if err != nil {
			detail := safeUpstreamError(err)
			s.log.Printf("UP channel=%s request failed: %s", route.ChannelID, detail)
			writeRetryableJSONError(w, http.StatusBadGateway, "upstream: "+detail)
			return
		}
		s.log.Printf("UP channel=%s status=%d ct=%s %s", route.ChannelID, response.StatusCode, response.Header.Get("Content-Type"), time.Since(started).Round(time.Millisecond))
		if encoding := strings.TrimSpace(response.Header.Get("Content-Encoding")); encoding != "" && !strings.EqualFold(encoding, "identity") {
			_ = response.Body.Close()
			writeJSONError(w, http.StatusBadGateway, "upstream returned unsupported content encoding "+encoding)
			return
		}
		if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
			break
		}
		if response.StatusCode >= http.StatusMultipleChoices && response.StatusCode < http.StatusBadRequest {
			_ = response.Body.Close()
			writeJSONError(w, http.StatusBadGateway, "upstream redirects are not accepted")
			return
		}

		data, readErr := readBodyLimited(response.Body, maxFacadeBodyBytes)
		_ = response.Body.Close()
		if readErr != nil {
			if errors.Is(readErr, errBodyTooLarge) {
				writeJSONError(w, http.StatusBadGateway, "upstream error body exceeds 64 MiB")
			} else {
				writeRetryableJSONError(w, http.StatusBadGateway, "read upstream error response: "+readErr.Error())
			}
			return
		}

		reasoningRejected := isOpaqueReasoningRejection(response.StatusCode, data)
		keptOpaqueReasoning := request.Reasoning.Opaque - request.Reasoning.Dropped
		if reasoningRejected && keptOpaqueReasoning > 0 && !request.ReasoningRecovery {
			retryRequest, retryErr := adaptFacadeRequestWithReasoning(body, route, incomingProtocol, s.reasoning, dropAllOpaqueReasoning)
			if retryErr == nil && retryRequest.Reasoning.Dropped > request.Reasoning.Dropped {
				s.log.Printf("UP channel=%s reasoning recovery retry once removed=%d after status=%d",
					route.ChannelID, retryRequest.Reasoning.Dropped, response.StatusCode)
				request = retryRequest
				saveLastRequestMeta(logTarget, route.WireModel, len(request.Body), tools, webSearch, hostedSearch, functionSearch, xSearch, request)
				continue
			}
			if retryErr != nil {
				s.log.Printf("UP channel=%s reasoning recovery request failed: %v", route.ChannelID, retryErr)
			}
		}

		copySafeResponseHeaders(w.Header(), response.Header)
		setRetryDisposition(w.Header(), response.StatusCode)
		if reasoningRejected {
			w.Header().Set("X-Should-Retry", "false")
		}
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(data)
		return
	}
	defer response.Body.Close()

	upstreamSSE := strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	if upstreamSSE && !request.Stream {
		writeJSONError(w, http.StatusBadGateway, "upstream ignored stream=false; cannot return an event stream")
		return
	}
	if upstreamSSE {
		if request.Kind == clientSearchRequest {
			writeJSONError(w, http.StatusBadGateway, "WebSearchClient requires one non-streaming Responses response")
			return
		}
		if request.IncomingProtocol != wireResponses {
			s.streamNativeSSE(w, response, route, request, started)
			return
		}
		switch request.Protocol {
		case wireResponses:
			options := patch.Options{GPTResponses: true, WebSearch: true, RequestModel: route.WireModel}
			s.streamResponsesSSE(w, response, route, request, options, started)
		case wireMessages:
			s.streamMessagesSSE(w, response, route, request, started)
		case wireChatCompletions:
			s.streamChatSSE(w, response, route, request, started)
		default:
			writeJSONError(w, http.StatusInternalServerError, "unsupported streaming backend")
		}
		return
	}

	data, readErr := readBodyLimited(response.Body, maxFacadeBodyBytes)
	if readErr != nil {
		if errors.Is(readErr, errBodyTooLarge) {
			writeJSONError(w, http.StatusBadGateway, "upstream response body exceeds 64 MiB")
		} else {
			writeRetryableJSONError(w, http.StatusBadGateway, "read upstream response: "+readErr.Error())
		}
		return
	}
	if isHTMLContentType(response.Header.Get("Content-Type")) {
		writeJSONError(w, http.StatusBadGateway, upstreamHTMLResponseMessage(route.APIBackend))
		return
	}

	if request.Kind == clientSearchRequest {
		s.writeClientSearchResponse(w, data, route, request)
		return
	}

	if request.IncomingProtocol == wireResponses {
		switch request.Protocol {
		case wireResponses:
			canonical, normalized, normalizeErr := s.normalizeResponsesJSON(data, route, request)
			if normalizeErr != nil {
				writeJSONError(w, http.StatusBadGateway, "invalid upstream Responses body: "+normalizeErr.Error())
				return
			}
			if request.Stream {
				s.log.Printf("UP channel=%s backend=responses ignored stream=true; emitting buffered JSON fallback", route.ChannelID)
				if err := writeCanonicalResponse(w, canonical, true); err != nil {
					writeJSONError(w, http.StatusBadGateway, "invalid non-stream Responses body: "+err.Error())
				}
				return
			}
			copySafeResponseHeaders(w.Header(), response.Header)
			writeJSONBody(w, response.StatusCode, normalized)
		case wireMessages, wireChatCompletions:
			s.writeTranslatedResponse(w, data, route, request)
		default:
			writeJSONError(w, http.StatusInternalServerError, "unsupported upstream protocol")
		}
		return
	}

	switch request.Protocol {
	case wireResponses:
		canonical, normalized, normalizeErr := s.normalizeResponsesJSON(data, route, request)
		if normalizeErr != nil {
			writeJSONError(w, http.StatusBadGateway, "invalid upstream Responses body: "+normalizeErr.Error())
			return
		}
		if request.Stream {
			s.log.Printf("UP channel=%s backend=responses ignored stream=true; emitting buffered JSON fallback", route.ChannelID)
			if err := writeCanonicalResponse(w, canonical, true); err != nil {
				writeJSONError(w, http.StatusBadGateway, "invalid non-stream Responses body: "+err.Error())
			}
			return
		}
		copySafeResponseHeaders(w.Header(), response.Header)
		writeJSONBody(w, response.StatusCode, normalized)
	case wireMessages:
		root, normalized, normalizeErr := s.normalizeNativeJSON(data, route, request, validateMessagesEnvelope)
		if normalizeErr != nil {
			writeJSONError(w, http.StatusBadGateway, "invalid upstream Messages body: "+normalizeErr.Error())
			return
		}
		if request.Stream {
			s.log.Printf("UP channel=%s backend=messages ignored stream=true; emitting buffered native SSE fallback", route.ChannelID)
			if err := writeMessagesSSEFallback(w, root); err != nil {
				writeJSONError(w, http.StatusBadGateway, "invalid non-stream Messages body: "+err.Error())
			}
			return
		}
		copySafeResponseHeaders(w.Header(), response.Header)
		writeJSONBody(w, response.StatusCode, normalized)
	case wireChatCompletions:
		root, normalized, normalizeErr := s.normalizeNativeJSON(data, route, request, validateChatEnvelope)
		if normalizeErr != nil {
			writeJSONError(w, http.StatusBadGateway, "invalid upstream Chat Completions body: "+normalizeErr.Error())
			return
		}
		if request.Stream {
			s.log.Printf("UP channel=%s backend=chat_completions ignored stream=true; emitting buffered native SSE fallback", route.ChannelID)
			if err := writeChatSSEFallback(w, root); err != nil {
				writeJSONError(w, http.StatusBadGateway, "invalid non-stream Chat Completions body: "+err.Error())
			}
			return
		}
		copySafeResponseHeaders(w.Header(), response.Header)
		writeJSONBody(w, response.StatusCode, normalized)
	}
}

func (s *Server) writeTranslatedResponse(w http.ResponseWriter, data []byte, route config.Route, request facadeRequest) {
	evidence := newSearchEvidence()
	evidence.observeJSON(data)
	s.logSearchEvidence(route.ChannelID, request, evidence)

	var result canonicalResult
	var err error
	switch request.Protocol {
	case wireMessages:
		result, err = canonicalFromMessages(data, request.HostedWebSearch, request.SearchQuery)
	case wireChatCompletions:
		result, err = canonicalFromChat(data, request.HostedWebSearch, request.SearchQuery)
	default:
		err = fmt.Errorf("unsupported translated protocol %q", request.Protocol)
	}
	if err != nil {
		s.log.Printf("UP channel=%s canonical response error: %v", route.ChannelID, err)
		writeJSONError(w, http.StatusBadGateway, "invalid upstream response: "+err.Error())
		return
	}
	canonical := canonicalResponse(route, request, result)
	restoreClientWebSearchAlias(canonical, request.ClientSearchAlias, wireResponses)
	backfillResponseSearchSources(canonical, request.HostedWebSearch, request.SearchQuery)
	if err := validateResponsesEnvelope(canonical); err != nil {
		writeJSONError(w, http.StatusBadGateway, "canonical response error: "+err.Error())
		return
	}
	s.captureReasoningProvenance(route, canonical)
	if request.Stream {
		s.log.Printf("UP channel=%s backend=%s ignored stream=true; emitting buffered Responses fallback", route.ChannelID, protocolLabel(request.Protocol))
	}
	if err := writeCanonicalResponse(w, canonical, request.Stream); err != nil {
		writeJSONError(w, http.StatusBadGateway, "canonical response error: "+err.Error())
	}
}

func requestKindLabel(kind facadeRequestKind) string {
	if kind == clientSearchRequest {
		return "client_search"
	}
	return "native_session"
}

func setRetryDisposition(header http.Header, status int) {
	if strings.TrimSpace(header.Get("X-Should-Retry")) != "" {
		return
	}
	retry := status == http.StatusTooManyRequests || status == http.StatusInternalServerError ||
		status == http.StatusBadGateway || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout
	header.Set("X-Should-Retry", fmt.Sprintf("%t", retry))
}

func (s *Server) normalizeResponsesJSON(data []byte, route config.Route, request facadeRequest) (map[string]any, []byte, error) {
	data, err := restoreClientWebSearchAliasJSON(data, request.ClientSearchAlias, wireResponses)
	if err != nil {
		return nil, nil, fmt.Errorf("restore client search alias: %w", err)
	}
	options := patch.Options{GPTResponses: true, WebSearch: true, RequestModel: route.WireModel}
	data, err = patch.PatchJSONBytesStrict(data, options)
	if err != nil {
		return nil, nil, err
	}
	root, err := decodeJSONMap(data)
	if err != nil {
		return nil, nil, err
	}
	backfillResponseSearchSources(root, request.HostedWebSearch, request.SearchQuery)
	if err := validateResponsesEnvelope(root); err != nil {
		return nil, nil, err
	}
	s.captureReasoningProvenance(route, root)
	normalized, err := json.Marshal(root)
	if err != nil {
		return nil, nil, err
	}
	evidence := newSearchEvidence()
	evidence.observeJSON(normalized)
	s.logSearchEvidence(route.ChannelID, request, evidence)
	return root, normalized, nil
}

func (s *Server) normalizeNativeJSON(
	data []byte,
	route config.Route,
	request facadeRequest,
	validate func(map[string]any) error,
) (map[string]any, []byte, error) {
	root, err := decodeJSONMap(data)
	if err != nil {
		return nil, nil, err
	}
	evidence := newSearchEvidence()
	evidence.observeJSON(data)
	if request.Protocol == wireMessages && request.HostedWebSearch {
		stripMessagesHostedSearchBlocks(root)
	}
	if err := validate(root); err != nil {
		return nil, nil, err
	}
	restoreClientWebSearchAlias(root, request.ClientSearchAlias, request.Protocol)
	s.captureReasoningProvenance(route, root)
	normalized, err := json.Marshal(root)
	s.logSearchEvidence(route.ChannelID, request, evidence)
	return root, normalized, err
}

func (s *Server) writeClientSearchResponse(w http.ResponseWriter, data []byte, route config.Route, request facadeRequest) {
	var canonical map[string]any
	var err error
	if request.Protocol == wireResponses {
		canonical, _, err = s.normalizeResponsesJSON(data, route, request)
	} else {
		var result canonicalResult
		if request.Protocol == wireMessages {
			result, err = canonicalFromMessages(data, true, request.SearchQuery)
		} else {
			result, err = canonicalFromChat(data, true, request.SearchQuery)
		}
		if err == nil {
			canonical = canonicalResponse(route, request, result)
			restoreClientWebSearchAlias(canonical, request.ClientSearchAlias, wireResponses)
			backfillResponseSearchSources(canonical, true, request.SearchQuery)
			err = validateResponsesEnvelope(canonical)
		}
		if err == nil {
			s.captureReasoningProvenance(route, canonical)
			evidence := newSearchEvidence()
			encoded, _ := json.Marshal(canonical)
			evidence.observeJSON(encoded)
			s.logSearchEvidence(route.ChannelID, request, evidence)
		}
	}
	if err == nil {
		err = validateClientSearchOutput(canonical)
	}
	if err != nil {
		s.log.Printf("UP channel=%s client search response error: %v", route.ChannelID, err)
		writeJSONError(w, http.StatusBadGateway, "invalid upstream client search response: "+err.Error())
		return
	}
	if err := writeCanonicalResponse(w, canonical, false); err != nil {
		writeJSONError(w, http.StatusBadGateway, "canonical client search response: "+err.Error())
	}
}

func validateClientSearchOutput(response map[string]any) error {
	hasCompletedSearch := false
	hasOutputText := false
	for _, raw := range anySlice(response["output"]) {
		item, _ := raw.(map[string]any)
		switch stringValue(item["type"]) {
		case "web_search_call":
			hasCompletedSearch = hasCompletedSearch || stringValue(item["status"]) == "completed"
		case "message":
			for _, rawPart := range anySlice(item["content"]) {
				part, _ := rawPart.(map[string]any)
				if stringValue(part["type"]) == "output_text" && strings.TrimSpace(stringValue(part["text"])) != "" {
					hasOutputText = true
				}
			}
		}
	}
	if !hasCompletedSearch {
		return fmt.Errorf("response contains no completed web_search_call; upstream may have ignored the search extension")
	}
	if !hasOutputText {
		return fmt.Errorf("response contains no non-empty output_text")
	}
	return nil
}

func writeJSONBody(w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func readBodyLimited(reader io.Reader, limit int64) ([]byte, error) {
	limited := &io.LimitedReader{R: reader, N: limit + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errBodyTooLarge
	}
	return data, nil
}

func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return false
	}
	mediaType = strings.ToLower(mediaType)
	return mediaType == "application/json" || strings.HasSuffix(mediaType, "+json")
}

func isHTMLContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && (strings.EqualFold(mediaType, "text/html") || strings.EqualFold(mediaType, "application/xhtml+xml"))
}

func upstreamHTMLResponseMessage(backend string) string {
	return fmt.Sprintf("upstream returned HTML instead of a %s JSON response; check the channel base_url API prefix and api_backend", backend)
}

func isLoopbackRequestHost(value string) bool {
	host := strings.TrimSpace(value)
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func routeIsLoopback(route config.Route) bool {
	return isLoopbackRequestHost(route.Host)
}

func routeHasCredential(route config.Route, incoming http.Header) bool {
	if strings.TrimSpace(route.APIKey) != "" ||
		headerValue(route.ExtraHeaders, "Authorization") != "" ||
		headerValue(route.ExtraHeaders, "X-Api-Key") != "" {
		return true
	}
	_, ok := incomingProviderCredential(route, incoming)
	return ok
}

func routeUsesIncomingProviderAuth(route config.Route) bool {
	return route.DynamicAuth && strings.TrimSpace(route.APIKey) == "" &&
		headerValue(route.ExtraHeaders, "Authorization") == "" &&
		headerValue(route.ExtraHeaders, "X-Api-Key") == ""
}

func applyRouteHeaders(header http.Header, route config.Route, protocol wireProtocol, incoming http.Header) {
	header.Del("Authorization")
	header.Del("X-Api-Key")
	authScheme := route.AuthScheme
	if protocol == wireMessages {
		if native, _ := routeProtocol(route); native == wireChatCompletions && isOfficialDeepSeekHost(route.Host) {
			authScheme = "x_api_key"
		}
	}
	if route.APIKey != "" {
		if authScheme == "x_api_key" {
			header.Set("X-Api-Key", route.APIKey)
		} else {
			header.Set("Authorization", "Bearer "+route.APIKey)
		}
	}
	for key, value := range route.ExtraHeaders {
		header.Set(key, value)
	}
	if token, ok := incomingProviderCredential(route, incoming); ok &&
		header.Get("Authorization") == "" && header.Get("X-Api-Key") == "" {
		if authScheme == "x_api_key" {
			header.Set("X-Api-Key", token)
		} else {
			header.Set("Authorization", "Bearer "+token)
		}
	}
	if authScheme == "x_api_key" && header.Get("X-Api-Key") == "" {
		if authorization := strings.TrimSpace(header.Get("Authorization")); authorization != "" {
			token := strings.TrimSpace(strings.TrimPrefix(authorization, "Bearer "))
			if token != "" {
				header.Set("X-Api-Key", token)
			}
		}
		header.Del("Authorization")
	}
}

func incomingProviderCredential(route config.Route, incoming http.Header) (string, bool) {
	if !routeUsesIncomingProviderAuth(route) {
		return "", false
	}
	switch route.IncomingAuthScheme {
	case "x_api_key":
		value := strings.TrimSpace(incoming.Get("X-Api-Key"))
		return value, value != ""
	default:
		value := strings.TrimSpace(incoming.Get("Authorization"))
		if len(value) < len("Bearer ") || !strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
			return "", false
		}
		value = strings.TrimSpace(value[len("Bearer "):])
		return value, value != ""
	}
}

func hasIncomingCredential(header http.Header) bool {
	return strings.TrimSpace(header.Get("Authorization")) != "" ||
		strings.TrimSpace(header.Get("X-Api-Key")) != "" ||
		strings.TrimSpace(header.Get("X-Xai-Token-Auth")) != ""
}

func headerValue(values map[string]string, name string) string {
	for key, value := range values {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func safeDiagnosticTarget(target string) string {
	parsed, err := url.Parse(target)
	if err != nil {
		return "<invalid upstream URL>"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	path := strings.ToLower(strings.TrimRight(parsed.Path, "/"))
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
		parsed.Path = "/.../chat/completions"
	case strings.HasSuffix(path, "/responses"):
		parsed.Path = "/.../responses"
	case strings.HasSuffix(path, "/messages"):
		parsed.Path = "/.../messages"
	default:
		parsed.Path = ""
	}
	parsed.RawPath = ""
	return parsed.String()
}

func safeUpstreamError(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Sprintf("%s %s: %v", urlErr.Op, safeDiagnosticTarget(urlErr.URL), urlErr.Err)
	}
	return err.Error()
}

func copySafeRequestHeaders(dst, src http.Header) {
	blocked := map[string]bool{
		"authorization": true, "proxy-authorization": true, "x-api-key": true,
		"x-xai-token-auth": true, "cookie": true, "set-cookie": true,
		"host": true, "content-length": true, "accept-encoding": true,
		"connection": true, "proxy-connection": true, "keep-alive": true,
		"te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
	}
	for _, named := range src.Values("Connection") {
		for _, key := range strings.Split(named, ",") {
			blocked[strings.ToLower(strings.TrimSpace(key))] = true
		}
	}
	for key, values := range src {
		if blocked[strings.ToLower(key)] {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copySafeResponseHeaders(dst, src http.Header) {
	blocked := map[string]bool{
		"content-length": true, "content-encoding": true, "transfer-encoding": true,
		"connection": true, "proxy-connection": true, "proxy-authenticate": true,
		"proxy-authorization": true, "keep-alive": true, "te": true,
		"trailer": true, "upgrade": true, "set-cookie": true, "location": true,
	}
	for _, named := range src.Values("Connection") {
		for _, key := range strings.Split(named, ",") {
			blocked[strings.ToLower(strings.TrimSpace(key))] = true
		}
	}
	for key, values := range src {
		if blocked[strings.ToLower(key)] {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
