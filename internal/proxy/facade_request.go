package proxy

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/hellowind777/hellogrok/internal/config"
)

type facadeRequestKind uint8

const (
	nativeSessionRequest facadeRequestKind = iota
	clientSearchRequest
)

type facadeRequest struct {
	Body                 []byte
	Kind                 facadeRequestKind
	IncomingProtocol     wireProtocol
	Protocol             wireProtocol
	Stream               bool
	HostedWebSearch      bool
	SearchQuery          string
	BuildHostedWebSearch int
	BuildXSearch         int
	ProxyAddedWebSearch  bool
	ClientSearchPrepared bool
	ClientSearchAlias    string
	Reasoning            reasoningFilterStats
	ReasoningRecovery    bool
}

func channelFromPath(escapedPath string) (string, wireProtocol, bool) {
	parts := strings.Split(strings.Trim(escapedPath, "/"), "/")
	if len(parts) < 3 || parts[0] != "c" {
		return "", wireUnknown, false
	}
	id, err := url.PathUnescape(parts[1])
	if err != nil || strings.TrimSpace(id) == "" {
		return "", wireUnknown, false
	}
	var protocol wireProtocol
	switch {
	case len(parts) == 3 && parts[2] == "responses":
		protocol = wireResponses
	case len(parts) == 3 && parts[2] == "messages":
		protocol = wireMessages
	case len(parts) == 4 && parts[2] == "chat" && parts[3] == "completions":
		protocol = wireChatCompletions
	default:
		return "", wireUnknown, false
	}
	return id, protocol, true
}

func routeProtocol(route config.Route) (wireProtocol, error) {
	switch strings.ToLower(strings.TrimSpace(route.APIBackend)) {
	case "responses":
		return wireResponses, nil
	case "messages":
		return wireMessages, nil
	case "chat_completions", "":
		return wireChatCompletions, nil
	default:
		return wireUnknown, fmt.Errorf("unsupported api_backend %q", route.APIBackend)
	}
}

// effectiveRouteProtocol follows Grok Build's resolved request protocol when
// neither the model/provider config nor a hellogrok-projected capability fixes
// the upstream protocol. This lets remote catalog updates flow through without
// teaching the proxy each future model ID.
func effectiveRouteProtocol(route config.Route, incoming wireProtocol) (wireProtocol, error) {
	if !route.APIBackendConfigured && strings.TrimSpace(route.APIBackend) == "" && !route.SupportsBackendSearch {
		switch incoming {
		case wireResponses, wireMessages, wireChatCompletions:
			return incoming, nil
		default:
			return wireUnknown, fmt.Errorf("unsupported incoming protocol %q", incoming)
		}
	}
	return routeProtocol(route)
}

// providerSearchProtocol selects the provider API that can actually execute
// hosted search. An explicit protocol dialect bridges any configured backend;
// otherwise Responses and Messages remain native and Chat uses a host default.
func providerSearchProtocol(route config.Route) (wireProtocol, error) {
	native, err := routeProtocol(route)
	if err != nil {
		return native, err
	}
	switch route.ChatSearchDialect {
	case config.ChatSearchDialectResponses:
		return wireResponses, nil
	case config.ChatSearchDialectMessages:
		return wireMessages, nil
	}
	if native != wireChatCompletions {
		return native, nil
	}
	switch chatSearchDialect(route) {
	case config.ChatSearchDialectResponses:
		return wireResponses, nil
	case config.ChatSearchDialectMessages:
		return wireMessages, nil
	case config.ChatSearchDialectSearchParameters, config.ChatSearchDialectWebSearchOptions:
		return wireChatCompletions, nil
	default:
		return wireUnknown, fmt.Errorf("unsupported Chat search dialect %q", chatSearchDialect(route))
	}
}

func upstreamTarget(route config.Route, protocol wireProtocol, rawQuery string) (string, error) {
	u, err := url.Parse(route.OriginBase)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid upstream base_url")
	}
	basePath := strings.TrimRight(u.Path, "/")
	if isOfficialDeepSeekRoute(route) {
		basePath = trimDeepSeekProtocolRoot(basePath)
	}
	var suffix string
	switch protocol {
	case wireResponses:
		suffix = "/responses"
	case wireMessages:
		if isOfficialDeepSeekRoute(route) {
			suffix = "/anthropic/v1/messages"
		} else {
			suffix = "/messages"
		}
	case wireChatCompletions:
		suffix = "/chat/completions"
	default:
		return "", fmt.Errorf("unsupported upstream protocol %q", protocol)
	}
	u.Path = basePath + suffix
	u.RawPath = ""
	if rawQuery != "" {
		incoming, err := url.ParseQuery(rawQuery)
		if err != nil {
			return "", fmt.Errorf("invalid request query parameters: %w", err)
		}
		merged := u.Query()
		for key, values := range incoming {
			merged.Del(key)
			for _, value := range values {
				merged.Add(key, value)
			}
		}
		u.RawQuery = merged.Encode()
	}
	return u.String(), nil
}

func trimDeepSeekProtocolRoot(path string) string {
	for _, suffix := range []string{"/anthropic/v1", "/anthropic", "/v1"} {
		if strings.HasSuffix(path, suffix) {
			return strings.TrimSuffix(path, suffix)
		}
	}
	return path
}

func adaptFacadeRequest(body []byte, route config.Route, incoming wireProtocol) (facadeRequest, error) {
	return adaptFacadeRequestWithReasoning(body, route, incoming, nil, keepUnknownReasoning)
}

func adaptFacadeRequestWithReasoning(
	body []byte,
	route config.Route,
	incoming wireProtocol,
	provenance *reasoningProvenanceStore,
	filterMode reasoningFilterMode,
) (facadeRequest, error) {
	root, err := decodeRequestObject(body)
	if err != nil {
		return facadeRequest{}, fmt.Errorf("decode %s request: %w", protocolLabel(incoming), err)
	}
	native, err := effectiveRouteProtocol(route, incoming)
	if err != nil {
		return facadeRequest{}, err
	}
	stream, _ := root["stream"].(bool)
	_, _, buildHostedSearch, _, buildXSearch := summarizeBody(body)
	preparedSearch := false
	if incoming == wireResponses {
		preparedSearch = prepareClientSearchExecution(root, buildHostedSearch, buildXSearch)
	}
	// When supports_backend_search is absent locally, Grok Build may inherit it
	// from a newer remote catalog. The concrete hosted tool on a Responses
	// request is the authoritative capability signal available to the facade.
	catalogHostedSearch := incoming == wireResponses && !preparedSearch &&
		!route.SupportsBackendSearch && hasHostedSearchTool(root)
	expected := native
	if route.SupportsBackendSearch {
		expected = wireResponses
	}
	if incoming != expected && !(incoming == wireResponses && (preparedSearch || catalogHostedSearch)) {
		return facadeRequest{}, fmt.Errorf("channel %q expects %s while the facade is active; received %s",
			route.ChannelID, protocolLabel(expected), protocolLabel(incoming))
	}

	request := facadeRequest{
		Kind:                 nativeSessionRequest,
		IncomingProtocol:     incoming,
		Stream:               stream,
		BuildHostedWebSearch: buildHostedSearch,
		BuildXSearch:         buildXSearch,
		ClientSearchPrepared: preparedSearch,
		ReasoningRecovery:    filterMode == dropAllOpaqueReasoning,
	}

	// Capable sessions and Grok Build's dedicated WebSearchClient both arrive as
	// Responses. Convert that canonical request only at the provider boundary.
	if route.SupportsBackendSearch || catalogHostedSearch || preparedSearch {
		root["model"] = route.WireModel
		request.Reasoning = filterReasoningInput(root, route, provenance, filterMode)
		request.SearchQuery = lastUserText(root["input"])
		capabilities := hostedSearchCapabilities{Web: preparedSearch}
		if route.SupportsBackendSearch && !preparedSearch {
			capabilities = routeHostedSearchCapabilities(route)
			request.ProxyAddedWebSearch = ensureHostedSearch(root, capabilities)
		} else if catalogHostedSearch {
			capabilities = hostedSearchCapabilities{
				Web: true,
				X:   native == wireResponses && isOfficialXAIHost(route.Host),
			}
		}
		normalizeHostedSearchObject(root, capabilities)
		request.HostedWebSearch = hasHostedSearchTool(root)
		if err := validateResponsesToolHistory(root["input"]); err != nil {
			return facadeRequest{}, err
		}
		if preparedSearch {
			request.Kind = clientSearchRequest
		}
		searchEligible := request.HostedWebSearch && toolChoiceAllowsHostedSearch(root["tool_choice"])
		if !searchEligible {
			request.Protocol = native
		} else if catalogHostedSearch {
			request.Protocol = native
		} else {
			request.Protocol, err = providerSearchProtocol(route)
			if err != nil {
				return facadeRequest{}, err
			}
		}
		switch request.Protocol {
		case wireResponses:
			// Providers that do not implement this standard hint may ignore it. Sending
			// it keeps source discovery forward-compatible when support is added.
			includeResponsesWebSearchSources(root)
			normalizeDeepSeekRequest(root, route, request.Protocol)
			request.Body, err = encodeRequestObject(root)
		case wireMessages:
			var converted map[string]any
			converted, err = responsesToMessagesRequest(root)
			if err == nil {
				normalizeDeepSeekRequest(converted, route, request.Protocol)
				request.Body, err = encodeRequestObject(converted)
			}
		case wireChatCompletions:
			var converted map[string]any
			converted, err = responsesToChatRequest(root, route)
			if err == nil {
				normalizeDeepSeekRequest(converted, route, request.Protocol)
				request.Body, err = encodeRequestObject(converted)
			}
		}
		return request, err
	}

	root["model"] = route.WireModel
	request.Protocol = native
	request.Reasoning = filterReasoningRequest(root, native, route, provenance, filterMode)
	request.SearchQuery = lastUserTextForProtocol(root, native)
	if err := validateNativeToolHistory(root, native); err != nil {
		return facadeRequest{}, err
	}
	describeClientWebTools(root)
	request.ClientSearchAlias = chooseClientWebSearchWireAlias(root)
	if !aliasClientWebSearchOnWire(root, request.ClientSearchAlias, native) {
		request.ClientSearchAlias = ""
	}
	normalizeDeepSeekRequest(root, route, request.Protocol)
	request.Body, err = encodeRequestObject(root)
	return request, err
}

func protocolLabel(protocol wireProtocol) string {
	switch protocol {
	case wireResponses:
		return "Responses"
	case wireMessages:
		return "Messages"
	case wireChatCompletions:
		return "Chat Completions"
	default:
		return "unknown protocol"
	}
}

const clientWebSearchDescription = "Search the public web for information, sources, current facts, or URLs. Use this tool whenever the user asks to search, browse, look up, verify, or obtain up-to-date information. This invokes Grok Build's configured client web-search model. In all user-visible text, refer to this tool only as web_search and never mention its internal wire name. Do not use web_fetch as a substitute for web search."

const clientWebFetchDescription = "Fetch and read one specific URL that is already known. Do not use this tool to search, discover URLs, or fetch a search-engine results page; use web_search first for those tasks."

const clientSearchExecutionInstructions = "Execute the hosted web_search for the supplied query. Use no more than four search calls, then always return a concise final text synthesis. Include the relevant source titles and URLs in that final text. Never finish with only reasoning or tool-call items."

// describeClientWebTools makes Build's two client-side web tools unambiguous
// to third-party models. The caller's structured tool choice remains the only
// source of mandatory selection; otherwise the conversation model decides.
func describeClientWebTools(root map[string]any) {
	if root == nil {
		return
	}
	for _, raw := range anySlice(root["tools"]) {
		tool, _ := raw.(map[string]any)
		if tool == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(functionToolName(tool)))
		switch name {
		case "web_search":
			setFunctionToolDescription(tool, clientWebSearchDescription)
		case "web_fetch":
			setFunctionToolDescription(tool, clientWebFetchDescription)
		}
	}
}

// prepareClientSearchExecution recognizes the small, non-streaming hosted
// request emitted by Build's WebSearchClient after the main model calls the
// client web_search function. The added instruction asks agentic providers for
// the OutputText that Build consumes without changing Build's automatic tool
// choice into a permanently forced server-tool loop.
func prepareClientSearchExecution(root map[string]any, buildHostedSearch, buildXSearch int) bool {
	if root == nil || buildHostedSearch != 1 || buildXSearch != 0 || toolChoiceDisablesTools(root["tool_choice"]) {
		return false
	}
	if stream, _ := root["stream"].(bool); stream {
		return false
	}
	store, hasStore := root["store"].(bool)
	query, hasStringInput := root["input"].(string)
	if !hasStore || store || !hasStringInput || strings.TrimSpace(query) == "" {
		return false
	}
	tools := anySlice(root["tools"])
	if len(tools) != 1 {
		return false
	}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		typ := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		if !isHostedWebSearchType(typ) {
			return false
		}
	}
	if instructions := strings.TrimSpace(stringValue(root["instructions"])); instructions == "" {
		root["instructions"] = clientSearchExecutionInstructions
	} else if !strings.Contains(instructions, clientSearchExecutionInstructions) {
		root["instructions"] = instructions + "\n\n" + clientSearchExecutionInstructions
	}
	return true
}

func setFunctionToolDescription(tool map[string]any, description string) {
	if function, _ := tool["function"].(map[string]any); function != nil {
		function["description"] = description
		return
	}
	tool["description"] = description
}

func allowedToolNames(choice any) (map[string]struct{}, string, bool) {
	value, ok := choice.(map[string]any)
	if !ok || !strings.EqualFold(strings.TrimSpace(stringValue(value["type"])), "allowed_tools") {
		return nil, "", false
	}
	mode := strings.ToLower(strings.TrimSpace(stringValue(value["mode"])))
	if mode == "" {
		mode = "auto"
	}
	allowed := make(map[string]struct{})
	for _, raw := range anySlice(value["tools"]) {
		tool, _ := raw.(map[string]any)
		if tool == nil {
			continue
		}
		typ := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		if typ == "function" {
			if name := strings.ToLower(strings.TrimSpace(functionToolName(tool))); name != "" {
				allowed["function:"+name] = struct{}{}
			}
			continue
		}
		if typ == "x_search" || isHostedWebSearchType(typ) {
			allowed["hosted:web_search"] = struct{}{}
		}
	}
	return allowed, mode, true
}

func toolChoiceAllowsFunction(choice any, name string) bool {
	allowed, _, constrained := allowedToolNames(choice)
	if !constrained {
		return true
	}
	_, ok := allowed["function:"+strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func toolChoiceAllowsHostedSearch(choice any) bool {
	if toolChoiceDisablesTools(choice) {
		return false
	}
	allowed, _, constrained := allowedToolNames(choice)
	if constrained {
		if _, ok := allowed["hosted:web_search"]; ok {
			return true
		}
		_, webSearch := allowed["function:web_search"]
		_, xSearch := allowed["function:x_search"]
		return webSearch || xSearch
	}
	value, ok := choice.(map[string]any)
	if !ok {
		return true
	}
	typ := strings.ToLower(strings.TrimSpace(stringValue(value["type"])))
	switch {
	case typ == "", typ == "auto", typ == "required":
		return true
	case typ == "x_search", isHostedWebSearchType(typ):
		return true
	case typ == "function":
		name := strings.ToLower(strings.TrimSpace(functionToolName(value)))
		return name == "web_search" || name == "x_search"
	default:
		// A named non-search tool choice cannot execute hosted search in this
		// request, even when the declaration remains in the tools list.
		return false
	}
}

func routeHostedSearchCapabilities(route config.Route) hostedSearchCapabilities {
	protocol, _ := providerSearchProtocol(route)
	return hostedSearchCapabilities{
		Web: true,
		X:   protocol == wireResponses && isOfficialXAIHost(route.Host),
	}
}

// ensureHostedSearch exposes hosted tools only for channels whose effective
// route enables backend search. Capability-aware normalization below removes
// any unsupported member of the pair.
func ensureHostedSearch(root map[string]any, capabilities hostedSearchCapabilities) bool {
	if root == nil || toolChoiceDisablesTools(root["tool_choice"]) ||
		!toolChoiceAllowsHostedSearch(root["tool_choice"]) || hasHostedSearchTool(root) ||
		!capabilities.any() {
		return false
	}
	tools := anySlice(root["tools"])
	webAdded := false
	if capabilities.Web {
		tools = append(tools, map[string]any{"type": "web_search"})
		webAdded = true
	}
	if capabilities.X {
		tools = append(tools, map[string]any{"type": "x_search"})
	}
	root["tools"] = tools
	return webAdded
}

func hasHostedSearchTool(root map[string]any) bool {
	tools, _ := root["tools"].([]any)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		typ, _ := tool["type"].(string)
		if typ == "x_search" || isHostedWebSearchType(typ) {
			return true
		}
	}
	return false
}

const responsesWebSearchSourcesInclude = "web_search_call.action.sources"

// includeResponsesWebSearchSources asks a Responses provider for every URL
// consulted by hosted search, which lets Grok Build render native site counts.
func includeResponsesWebSearchSources(root map[string]any) bool {
	if root == nil {
		return false
	}
	hasWebSearch := false
	for _, raw := range anySlice(root["tools"]) {
		tool, _ := raw.(map[string]any)
		if tool != nil && isHostedWebSearchType(stringValue(tool["type"])) {
			hasWebSearch = true
			break
		}
	}
	if !hasWebSearch {
		return false
	}

	var includes []any
	if raw, exists := root["include"]; exists && raw != nil {
		var ok bool
		includes, ok = raw.([]any)
		if !ok {
			return false
		}
	}
	for _, value := range includes {
		if stringValue(value) == responsesWebSearchSourcesInclude {
			return false
		}
	}
	root["include"] = append(includes, responsesWebSearchSourcesInclude)
	return true
}

func responsesToMessagesRequest(root map[string]any) (map[string]any, error) {
	messages, system, err := responsesInputToMessages(root["input"], true)
	if err != nil {
		return nil, err
	}
	maxTokens := positiveInt(root["max_output_tokens"], 0)
	if maxTokens == 0 {
		return nil, fmt.Errorf("Responses max_output_tokens must be a positive integer when converting to Messages; configure max_completion_tokens or use model metadata that supplies it")
	}
	if instructions := stringValue(root["instructions"]); instructions != "" {
		system = append([]string{instructions}, system...)
	}
	out := map[string]any{
		"model":      stringValue(root["model"]),
		"messages":   messages,
		"max_tokens": maxTokens,
		"stream":     root["stream"] == true,
	}
	if len(system) > 0 {
		out["system"] = strings.Join(system, "\n\n")
	}
	copyIfPresent(out, root, "temperature", "top_p")
	if user, exists := root["user"]; exists && user != nil {
		out["metadata"] = map[string]any{"user_id": user}
	}

	if !toolChoiceDisablesTools(root["tool_choice"]) {
		tools, hosted := messagesTools(root["tools"], root["tool_choice"])
		if len(tools) > 0 {
			out["tools"] = tools
			if choice := messagesToolChoice(root["tool_choice"], hosted); choice != nil {
				out["tool_choice"] = choice
			}
		}
	}
	if reasoning, _ := root["reasoning"].(map[string]any); reasoning != nil {
		if effort := stringValue(reasoning["effort"]); effort != "" {
			out["thinking"] = map[string]any{"type": "adaptive", "display": "summarized"}
			out["output_config"] = map[string]any{"effort": effort}
		}
	}
	if text, _ := root["text"].(map[string]any); text != nil {
		if format, _ := text["format"].(map[string]any); format != nil && stringValue(format["type"]) == "json_schema" {
			schema := format["schema"]
			if schema == nil {
				if js, _ := format["json_schema"].(map[string]any); js != nil {
					schema = js["schema"]
				}
			}
			if schema != nil {
				cfg, _ := out["output_config"].(map[string]any)
				if cfg == nil {
					cfg = map[string]any{}
					out["output_config"] = cfg
				}
				cfg["format"] = map[string]any{"type": "json_schema", "schema": schema}
			}
		}
	}
	applyMessagesCacheBreakpoints(out)
	if err := validateMessagesToolHistory(out["messages"]); err != nil {
		return nil, fmt.Errorf("converted Messages request: %w", err)
	}
	return out, nil
}

// applyMessagesCacheBreakpoints mirrors Grok Build's Messages request builder:
// cache the system prompt, the current tip, and the user turn where the prior
// request ended. Keeping one provider slot free also avoids Anthropic's limit.
func applyMessagesCacheBreakpoints(root map[string]any) {
	if root == nil {
		return
	}
	cacheControl := func() map[string]any { return map[string]any{"type": "ephemeral"} }
	switch system := root["system"].(type) {
	case string:
		root["system"] = []any{map[string]any{
			"type": "text", "text": system, "cache_control": cacheControl(),
		}}
	case []any:
		for index := len(system) - 1; index >= 0; index-- {
			block, _ := system[index].(map[string]any)
			if block != nil && stringValue(block["type"]) == "text" {
				block["cache_control"] = cacheControl()
				break
			}
		}
	}

	messages, _ := root["messages"].([]any)
	tip := -1
	for index := len(messages) - 1; index >= 0; index-- {
		message, _ := messages[index].(map[string]any)
		if markMessagesCacheBreakpoint(message) {
			tip = index
			break
		}
	}
	if tip < 0 {
		return
	}
	previousAssistant := -1
	for index := tip - 1; index >= 0; index-- {
		message, _ := messages[index].(map[string]any)
		if strings.EqualFold(strings.TrimSpace(stringValue(message["role"])), "assistant") {
			previousAssistant = index
			break
		}
	}
	for index := previousAssistant - 1; index >= 0; index-- {
		message, _ := messages[index].(map[string]any)
		if strings.EqualFold(strings.TrimSpace(stringValue(message["role"])), "user") {
			markMessagesCacheBreakpoint(message)
			break
		}
	}
}

func markMessagesCacheBreakpoint(message map[string]any) bool {
	if message == nil {
		return false
	}
	cacheControl := map[string]any{"type": "ephemeral"}
	switch content := message["content"].(type) {
	case string:
		message["content"] = []any{map[string]any{
			"type": "text", "text": content, "cache_control": cacheControl,
		}}
		return true
	case []any:
		for index := len(content) - 1; index >= 0; index-- {
			block, _ := content[index].(map[string]any)
			switch stringValue(block["type"]) {
			case "text", "tool_result", "image", "tool_use":
				block["cache_control"] = cacheControl
				return true
			}
		}
	}
	return false
}

func responsesToChatRequest(root map[string]any, route config.Route) (map[string]any, error) {
	messages, _, err := responsesInputToMessages(root["input"], false)
	if err != nil {
		return nil, err
	}
	if instructions := stringValue(root["instructions"]); instructions != "" {
		messages = append([]any{map[string]any{"role": "system", "content": instructions}}, messages...)
	}
	out := map[string]any{
		"model":    stringValue(root["model"]),
		"messages": messages,
		"stream":   root["stream"] == true,
	}
	if root["stream"] == true {
		// OpenAI-compatible gateways report final token usage in a trailing
		// streaming chunk only when this option is enabled.
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if n := positiveInt(root["max_output_tokens"], 0); n > 0 {
		out["max_tokens"] = n
	}
	copyIfPresent(out, root, "temperature", "top_p", "user")

	var tools []any
	hosted := hasHostedSearchTool(root) && toolChoiceAllowsHostedSearch(root["tool_choice"])
	toolsDisabled := toolChoiceDisablesTools(root["tool_choice"])
	for _, raw := range anySlice(root["tools"]) {
		if toolsDisabled {
			break
		}
		tool, _ := raw.(map[string]any)
		typ := stringValue(tool["type"])
		if typ != "function" || !toolChoiceAllowsFunction(root["tool_choice"], functionToolName(tool)) ||
			(hosted && isSearchFunctionTool(tool)) {
			continue
		}
		fn := map[string]any{"name": tool["name"], "parameters": valueOr(tool["parameters"], map[string]any{"type": "object"})}
		if desc := stringValue(tool["description"]); desc != "" {
			fn["description"] = desc
		}
		if strict, ok := tool["strict"]; ok {
			fn["strict"] = strict
		}
		tools = append(tools, map[string]any{"type": "function", "function": fn})
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	if !toolsDisabled && len(tools) > 0 {
		if choice := chatToolChoice(root["tool_choice"], hosted); choice != nil {
			out["tool_choice"] = choice
		}
	}
	if hosted && !toolsDisabled {
		searchTool := firstHostedSearchTool(root["tools"])
		switch chatSearchDialect(route) {
		case config.ChatSearchDialectSearchParameters:
			mode := "auto"
			if hostedToolChoiceRequired(root["tool_choice"]) {
				mode = "on"
			}
			source := map[string]any{"type": "web"}
			copySearchDomainFilters(source, searchTool, "allowed_websites", "excluded_websites")
			out["search_parameters"] = map[string]any{"mode": mode, "sources": []any{source}}
		case config.ChatSearchDialectWebSearchOptions:
			options := map[string]any{}
			copyIfPresent(options, searchTool, "search_context_size", "user_location")
			out["web_search_options"] = options
		default:
			return nil, fmt.Errorf("Chat search dialect %q requires a protocol bridge", chatSearchDialect(route))
		}
	}
	if reasoning, _ := root["reasoning"].(map[string]any); reasoning != nil {
		if effort := stringValue(reasoning["effort"]); effort != "" {
			out["reasoning_effort"] = effort
		}
	}
	if err := validateChatToolHistory(out["messages"]); err != nil {
		return nil, fmt.Errorf("converted Chat Completions request: %w", err)
	}
	return out, nil
}

func firstHostedSearchTool(value any) map[string]any {
	for _, raw := range anySlice(value) {
		tool, _ := raw.(map[string]any)
		if tool != nil && (stringValue(tool["type"]) == "x_search" || isHostedWebSearchType(stringValue(tool["type"]))) {
			return tool
		}
	}
	return map[string]any{}
}

func responsesInputToMessages(input any, anthropic bool) ([]any, []string, error) {
	var messages []any
	var system []string
	var pendingReasoning []string
	chatAssistantIndex := -1
	if text, ok := input.(string); ok {
		content := any(text)
		if anthropic {
			// Grok Build's Messages builder always represents conversation text as
			// blocks. Keeping that shape even before a cache breakpoint is attached
			// prevents old history from changing representation when breakpoints move.
			content = []any{map[string]any{"type": "text", "text": text}}
		}
		return []any{map[string]any{"role": "user", "content": content}}, nil, nil
	}
	items := anySlice(input)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		typ := stringValue(item["type"])
		role := stringValue(item["role"])
		if typ == "" && role != "" {
			typ = "message"
		}
		switch typ {
		case "message":
			content := convertMessageContent(item["content"], anthropic)
			if role == "system" && anthropic {
				if text := contentText(content); text != "" {
					system = append(system, text)
				}
				continue
			}
			if anthropic && role == "assistant" {
				appendAnthropicAssistantContent(&messages, content)
			} else if !anthropic && role == "assistant" {
				appendChatAssistant(&messages, content, &pendingReasoning)
				chatAssistantIndex = len(messages) - 1
			} else {
				pendingReasoning = nil
				chatAssistantIndex = -1
				appendMessage(&messages, role, content)
			}
		case "function_call":
			id := firstString(item, "call_id", "id")
			name := stringValue(item["name"])
			arguments := stringValue(item["arguments"])
			if anthropic {
				var input any = map[string]any{}
				if arguments != "" {
					decoded, err := decodeRequestObject([]byte(arguments))
					if err != nil {
						return nil, nil, fmt.Errorf("function call %q arguments must be one JSON object: %w", name, err)
					}
					input = decoded
				}
				appendBlockToRole(&messages, "assistant", map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
			} else {
				appendChatToolCall(&messages, map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": arguments}}, &pendingReasoning, &chatAssistantIndex)
			}
		case "function_call_output":
			id := firstString(item, "call_id", "id")
			output := responseOutputContent(item["output"], anthropic)
			pendingReasoning = nil
			chatAssistantIndex = -1
			if anthropic {
				appendBlockToRole(&messages, "user", map[string]any{"type": "tool_result", "tool_use_id": id, "content": output})
			} else {
				messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": output})
			}
		case "reasoning":
			text := reasoningInputText(item)
			if anthropic {
				signature := stringValue(item["encrypted_content"])
				if text != "" || signature != "" {
					appendBlockToRole(&messages, "assistant", map[string]any{"type": "thinking", "thinking": text, "signature": signature})
				}
			} else if text != "" {
				pendingReasoning = append(pendingReasoning, text)
				chatAssistantIndex = -1
			}
		case "web_search_call":
			if anthropic {
				// Match Grok Build's native Messages conversion: backend calls have
				// no lossless Messages history equivalent, so retain their stable
				// human-readable summary in the assistant turn. The live call/result
				// still uses provider-native server tool blocks on the response path.
				if summary := backendToolSummary(item); summary != "" {
					appendBlockToRole(&messages, "assistant", map[string]any{"type": "text", "text": summary})
				}
				continue
			}
			summary := backendToolSummary(item)
			if summary == "" {
				continue
			}
			appendMessage(&messages, "assistant", summary)
			chatAssistantIndex = -1
		case "custom_tool_call", "code_interpreter_call":
			summary := backendToolSummary(item)
			if summary == "" {
				continue
			}
			if anthropic {
				appendBlockToRole(&messages, "assistant", map[string]any{"type": "text", "text": summary})
			} else {
				appendMessage(&messages, "assistant", summary)
				chatAssistantIndex = -1
			}
		}
	}
	return messages, system, nil
}

func appendAnthropicAssistantContent(messages *[]any, content any) {
	if text, ok := content.(string); ok {
		if text != "" {
			appendBlockToRole(messages, "assistant", map[string]any{"type": "text", "text": text})
		}
		return
	}
	for _, raw := range anySlice(content) {
		if block, ok := raw.(map[string]any); ok {
			appendBlockToRole(messages, "assistant", block)
		}
	}
}

func appendChatAssistant(messages *[]any, content any, pendingReasoning *[]string) {
	message := map[string]any{"role": "assistant", "content": content}
	if len(*pendingReasoning) > 0 {
		message["reasoning_content"] = strings.Join(*pendingReasoning, "\n")
		*pendingReasoning = nil
	}
	*messages = append(*messages, message)
}

func reasoningInputText(item map[string]any) string {
	var parts []string
	for _, field := range []string{"summary", "content"} {
		for _, raw := range anySlice(item[field]) {
			part, _ := raw.(map[string]any)
			if text := stringValue(part["text"]); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func backendToolSummary(item map[string]any) string {
	switch stringValue(item["type"]) {
	case "web_search_call":
		action, _ := item["action"].(map[string]any)
		switch stringValue(action["type"]) {
		case "search", "":
			query := stringValue(action["query"])
			if query == "" {
				query = firstSearchQuery(action["queries"])
			}
			return "[backend web_search] search: " + query
		case "open_page", "open":
			url := stringValue(action["url"])
			if url == "" {
				url = "?"
			}
			return "[backend web_search] open: " + url
		case "find", "find_in_page":
			return fmt.Sprintf("[backend web_search] find %q in %s", stringValue(action["pattern"]), stringValue(action["url"]))
		}
	case "custom_tool_call":
		return fmt.Sprintf("[backend x_search] %s(%s)", stringValue(item["name"]), stringValue(item["input"]))
	case "code_interpreter_call":
		code := stringValue(item["code"])
		if len(code) > 100 {
			code = code[:100] + "..."
		}
		return "[backend code_interpreter] " + code
	}
	return ""
}

func validateNativeToolHistory(root map[string]any, protocol wireProtocol) error {
	switch protocol {
	case wireResponses:
		return validateResponsesToolHistory(root["input"])
	case wireMessages:
		return validateMessagesToolHistory(root["messages"])
	case wireChatCompletions:
		return validateChatToolHistory(root["messages"])
	default:
		return fmt.Errorf("unsupported request protocol")
	}
}

func validateResponsesToolHistory(value any) error {
	if _, ok := value.(string); ok {
		return nil
	}
	items, ok := value.([]any)
	if !ok {
		return fmt.Errorf("Responses request input must be a string or array")
	}
	open := map[string]struct{}{}
	seenCalls := map[string]struct{}{}
	seenResults := map[string]struct{}{}
	order := []string{}
	flush := func() error {
		if len(open) == 0 {
			return nil
		}
		return fmt.Errorf("Responses tool history is invalid: missing function_call_output for %s", strings.Join(unresolvedIDs(order, open), ", "))
	}
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		typ := stringValue(item["type"])
		if typ != "function_call" && typ != "function_call_output" {
			if err := flush(); err != nil {
				return err
			}
			order = nil
			continue
		}
		id := firstString(item, "call_id")
		if id == "" {
			return fmt.Errorf("Responses tool history is invalid: %s has no call_id", typ)
		}
		if typ == "function_call" {
			if len(open) == 0 {
				order = nil
			}
			if _, duplicate := seenCalls[id]; duplicate {
				return fmt.Errorf("Responses tool history is invalid: duplicate function_call id %s", id)
			}
			seenCalls[id] = struct{}{}
			open[id] = struct{}{}
			order = append(order, id)
			continue
		}
		if _, duplicate := seenResults[id]; duplicate {
			return fmt.Errorf("Responses tool history is invalid: duplicate function_call_output for %s", id)
		}
		if _, found := open[id]; !found {
			return fmt.Errorf("Responses tool history is invalid: function_call_output %s has no preceding unresolved function_call", id)
		}
		seenResults[id] = struct{}{}
		delete(open, id)
	}
	return flush()
}

func validateMessagesToolHistory(value any) error {
	messages, ok := value.([]any)
	if !ok {
		return fmt.Errorf("Messages request messages must be an array")
	}
	seenCalls := map[string]struct{}{}
	seenResults := map[string]struct{}{}
	var pending map[string]struct{}
	var order []string
	for index, raw := range messages {
		message, _ := raw.(map[string]any)
		if message == nil {
			return fmt.Errorf("Messages request messages[%d] must be an object", index)
		}
		role := stringValue(message["role"])
		blocks := contentBlocks(message["content"])
		if len(pending) > 0 {
			if role != "user" {
				return fmt.Errorf("Messages tool history is invalid: tool_result blocks for %s must be in the immediately following user message", strings.Join(unresolvedIDs(order, pending), ", "))
			}
			sawNonResult := false
			for _, block := range blocks {
				if stringValue(block["type"]) != "tool_result" {
					sawNonResult = true
					continue
				}
				if sawNonResult {
					return fmt.Errorf("Messages tool history is invalid: tool_result blocks must precede other user content")
				}
				id := stringValue(block["tool_use_id"])
				if id == "" {
					return fmt.Errorf("Messages tool history is invalid: tool_result has no tool_use_id")
				}
				if _, duplicate := seenResults[id]; duplicate {
					return fmt.Errorf("Messages tool history is invalid: duplicate tool_result for %s", id)
				}
				if _, found := pending[id]; !found {
					return fmt.Errorf("Messages tool history is invalid: tool_result %s does not match the immediately preceding assistant tool_use batch", id)
				}
				seenResults[id] = struct{}{}
				delete(pending, id)
			}
			if len(pending) > 0 {
				return fmt.Errorf("Messages tool history is invalid: missing tool_result for %s", strings.Join(unresolvedIDs(order, pending), ", "))
			}
			pending, order = nil, nil
		} else {
			for _, block := range blocks {
				if stringValue(block["type"]) == "tool_result" {
					return fmt.Errorf("Messages tool history is invalid: tool_result %s has no immediately preceding assistant tool_use", stringValue(block["tool_use_id"]))
				}
			}
		}
		if role != "assistant" {
			continue
		}
		for _, block := range blocks {
			if stringValue(block["type"]) != "tool_use" {
				continue
			}
			id := stringValue(block["id"])
			if id == "" {
				return fmt.Errorf("Messages tool history is invalid: tool_use has no id")
			}
			if _, duplicate := seenCalls[id]; duplicate {
				return fmt.Errorf("Messages tool history is invalid: duplicate tool_use id %s", id)
			}
			if pending == nil {
				pending = map[string]struct{}{}
			}
			seenCalls[id] = struct{}{}
			pending[id] = struct{}{}
			order = append(order, id)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("Messages tool history is invalid: missing tool_result for %s", strings.Join(unresolvedIDs(order, pending), ", "))
	}
	return nil
}

func validateChatToolHistory(value any) error {
	messages, ok := value.([]any)
	if !ok {
		return fmt.Errorf("Chat Completions request messages must be an array")
	}
	seenCalls := map[string]struct{}{}
	seenResults := map[string]struct{}{}
	var pending map[string]struct{}
	var order []string
	for index, raw := range messages {
		message, _ := raw.(map[string]any)
		if message == nil {
			return fmt.Errorf("Chat Completions request messages[%d] must be an object", index)
		}
		role := stringValue(message["role"])
		if role == "tool" {
			id := stringValue(message["tool_call_id"])
			if id == "" {
				return fmt.Errorf("Chat Completions tool history is invalid: tool message has no tool_call_id")
			}
			if _, duplicate := seenResults[id]; duplicate {
				return fmt.Errorf("Chat Completions tool history is invalid: duplicate tool result for %s", id)
			}
			if _, found := pending[id]; !found {
				return fmt.Errorf("Chat Completions tool history is invalid: tool result %s has no immediately preceding assistant tool_call batch", id)
			}
			seenResults[id] = struct{}{}
			delete(pending, id)
			continue
		}
		if len(pending) > 0 {
			return fmt.Errorf("Chat Completions tool history is invalid: missing tool result for %s", strings.Join(unresolvedIDs(order, pending), ", "))
		}
		pending, order = nil, nil
		if role != "assistant" {
			continue
		}
		for _, rawCall := range anySlice(message["tool_calls"]) {
			call, _ := rawCall.(map[string]any)
			id := stringValue(call["id"])
			if id == "" {
				return fmt.Errorf("Chat Completions tool history is invalid: tool_call has no id")
			}
			if _, duplicate := seenCalls[id]; duplicate {
				return fmt.Errorf("Chat Completions tool history is invalid: duplicate tool_call id %s", id)
			}
			if pending == nil {
				pending = map[string]struct{}{}
			}
			seenCalls[id] = struct{}{}
			pending[id] = struct{}{}
			order = append(order, id)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("Chat Completions tool history is invalid: missing tool result for %s", strings.Join(unresolvedIDs(order, pending), ", "))
	}
	return nil
}

func unresolvedIDs(order []string, unresolved map[string]struct{}) []string {
	missing := make([]string, 0, len(unresolved))
	for _, id := range order {
		if _, ok := unresolved[id]; ok {
			missing = append(missing, id)
		}
	}
	return missing
}

func contentBlocks(value any) []map[string]any {
	blocks := make([]map[string]any, 0)
	for _, raw := range anySlice(value) {
		if block, ok := raw.(map[string]any); ok && block != nil {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

func lastUserTextForProtocol(root map[string]any, protocol wireProtocol) string {
	if protocol == wireResponses {
		return lastUserText(root["input"])
	}
	messages := anySlice(root["messages"])
	for index := len(messages) - 1; index >= 0; index-- {
		message, _ := messages[index].(map[string]any)
		if stringValue(message["role"]) == "user" {
			return contentText(message["content"])
		}
	}
	return ""
}

func firstSearchQuery(value any) string {
	for _, raw := range anySlice(value) {
		if query := strings.TrimSpace(stringValue(raw)); query != "" && !strings.HasPrefix(query, "ws_call_id=") {
			return query
		}
	}
	return ""
}

func convertMessageContent(value any, anthropic bool) any {
	if text, ok := value.(string); ok {
		if anthropic {
			return []any{map[string]any{"type": "text", "text": text}}
		}
		return text
	}
	var blocks []any
	for _, raw := range anySlice(value) {
		part, _ := raw.(map[string]any)
		if part == nil {
			continue
		}
		typ := stringValue(part["type"])
		switch typ {
		case "input_text", "output_text", "text":
			blocks = append(blocks, map[string]any{"type": "text", "text": stringValue(part["text"])})
		case "input_image", "image_url":
			imageURL := stringValue(part["image_url"])
			if imageMap, _ := part["image_url"].(map[string]any); imageURL == "" && imageMap != nil {
				imageURL = stringValue(imageMap["url"])
			}
			if anthropic {
				blocks = append(blocks, anthropicImageBlock(imageURL))
			} else {
				blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}})
			}
		}
	}
	if !anthropic && len(blocks) == 1 {
		if block, _ := blocks[0].(map[string]any); block != nil && stringValue(block["type"]) == "text" {
			return stringValue(block["text"])
		}
	}
	return blocks
}

func anthropicImageBlock(raw string) map[string]any {
	if strings.HasPrefix(raw, "data:") {
		header, data, ok := strings.Cut(raw, ",")
		if ok {
			media := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
			return map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": media, "data": data}}
		}
	}
	return map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": raw}}
}

func messagesTools(value, choice any) ([]any, bool) {
	var tools []any
	hosted := false
	for _, raw := range anySlice(value) {
		tool, _ := raw.(map[string]any)
		typ := stringValue(tool["type"])
		if (typ == "x_search" || isHostedWebSearchType(typ)) && toolChoiceAllowsHostedSearch(choice) {
			hosted = true
			break
		}
	}
	for _, raw := range anySlice(value) {
		tool, _ := raw.(map[string]any)
		typ := stringValue(tool["type"])
		switch {
		case typ == "function":
			if !toolChoiceAllowsFunction(choice, functionToolName(tool)) {
				continue
			}
			if hosted && isSearchFunctionTool(tool) {
				continue
			}
			entry := map[string]any{"name": tool["name"], "input_schema": valueOr(tool["parameters"], map[string]any{"type": "object"})}
			if desc := stringValue(tool["description"]); desc != "" {
				entry["description"] = desc
			}
			tools = append(tools, entry)
		case (typ == "x_search" || isHostedWebSearchType(typ)) && hosted:
			if !containsMessagesHostedSearch(tools) {
				tools = append(tools, messagesSearchTool(tool))
			}
		}
	}
	return tools, hosted
}

func containsMessagesHostedSearch(tools []any) bool {
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if stringValue(tool["name"]) == "web_search" && isHostedWebSearchType(stringValue(tool["type"])) {
			return true
		}
	}
	return false
}

func messagesSearchTool(tool map[string]any) map[string]any {
	out := map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 10}
	copySearchDomainFilters(out, tool, "allowed_domains", "blocked_domains")
	if value := positiveInt(tool["max_uses"], 0); value > 0 {
		out["max_uses"] = value
	}
	if location, _ := tool["user_location"].(map[string]any); location != nil {
		out["user_location"] = cloneMap(location)
	}
	return out
}

func copySearchDomainFilters(dst, tool map[string]any, allowedKey, blockedKey string) {
	filters, _ := tool["filters"].(map[string]any)
	for _, entry := range []struct {
		source string
		target string
	}{{"allowed_domains", allowedKey}, {"blocked_domains", blockedKey}} {
		value := tool[entry.source]
		if value == nil && filters != nil {
			value = filters[entry.source]
		}
		if domains := nonEmptyStringSlice(value); len(domains) > 0 {
			dst[entry.target] = domains
		}
	}
}

func nonEmptyStringSlice(value any) []string {
	var out []string
	for _, raw := range anySlice(value) {
		if text, ok := raw.(string); ok && strings.TrimSpace(text) != "" {
			out = append(out, strings.TrimSpace(text))
		}
	}
	return out
}

func chatSearchDialect(route config.Route) config.ChatSearchDialect {
	if route.ChatSearchDialect != "" {
		return route.ChatSearchDialect
	}
	if isOfficialDeepSeekRoute(route) || isOfficialXAIHost(route.Host) {
		return config.ChatSearchDialectResponses
	}
	return config.ChatSearchDialectWebSearchOptions
}

func normalizedHostname(value string) string {
	host := strings.ToLower(strings.TrimSpace(value))
	if parsedHost, _, err := net.SplitHostPort(host); err == nil {
		host = parsedHost
	}
	return strings.Trim(host, "[]")
}

func isOfficialXAIHost(value string) bool {
	host := normalizedHostname(value)
	return host == "x.ai" || strings.HasSuffix(host, ".x.ai")
}

func messagesToolChoice(value any, hosted bool) any {
	switch choice := value.(type) {
	case string:
		switch choice {
		case "required":
			return map[string]any{"type": "any"}
		case "none":
			return map[string]any{"type": "auto", "disable_parallel_tool_use": true}
		case "auto":
			return map[string]any{"type": "auto"}
		}
	case map[string]any:
		typ := stringValue(choice["type"])
		if typ == "allowed_tools" {
			_, mode, _ := allowedToolNames(choice)
			if mode == "required" {
				return map[string]any{"type": "any"}
			}
			return map[string]any{"type": "auto"}
		}
		if typ == "function" {
			name := stringValue(choice["name"])
			if (name == "web_search" || name == "x_search") && hosted {
				return map[string]any{"type": "tool", "name": "web_search"}
			}
			return map[string]any{"type": "tool", "name": name}
		}
		if (typ == "web_search" || typ == "x_search") && hosted {
			return map[string]any{"type": "tool", "name": "web_search"}
		}
	}
	return nil
}

func chatToolChoice(value any, hosted bool) any {
	if hosted && value == "required" {
		// Chat has no portable "any function or hosted search" choice. The
		// provider search extension is forced separately, so do not also force
		// an unrelated function call.
		return nil
	}
	choice, ok := value.(map[string]any)
	if !ok {
		return value
	}
	if stringValue(choice["type"]) == "allowed_tools" {
		_, mode, _ := allowedToolNames(choice)
		if mode == "required" {
			if hosted {
				return nil
			}
			return "required"
		}
		return "auto"
	}
	if stringValue(choice["type"]) == "function" {
		name := stringValue(choice["name"])
		if hosted && (name == "web_search" || name == "x_search") {
			return nil
		}
		return map[string]any{"type": "function", "function": map[string]any{"name": name}}
	}
	return nil
}

func hostedToolChoiceRequired(value any) bool {
	choice, ok := value.(map[string]any)
	if !ok {
		return value == "required"
	}
	typ := stringValue(choice["type"])
	if typ == "allowed_tools" {
		_, mode, _ := allowedToolNames(choice)
		return mode == "required" && toolChoiceAllowsHostedSearch(choice)
	}
	name := stringValue(choice["name"])
	return typ == "x_search" || isHostedWebSearchType(typ) ||
		(typ == "function" && (name == "web_search" || name == "x_search"))
}

func appendMessage(messages *[]any, role string, content any) {
	if role == "" {
		role = "user"
	}
	*messages = append(*messages, map[string]any{"role": role, "content": content})
}

func appendBlockToRole(messages *[]any, role string, block map[string]any) {
	if len(*messages) > 0 {
		last, _ := (*messages)[len(*messages)-1].(map[string]any)
		if stringValue(last["role"]) == role {
			content := anySlice(last["content"])
			if text, ok := last["content"].(string); ok && text != "" {
				content = []any{map[string]any{"type": "text", "text": text}}
			}
			last["content"] = append(content, block)
			return
		}
	}
	appendMessage(messages, role, []any{block})
}

func appendChatToolCall(messages *[]any, call map[string]any, pendingReasoning *[]string, assistantIndex *int) {
	if *assistantIndex >= 0 && *assistantIndex == len(*messages)-1 && len(*pendingReasoning) == 0 {
		last, _ := (*messages)[*assistantIndex].(map[string]any)
		if stringValue(last["role"]) == "assistant" {
			last["tool_calls"] = append(anySlice(last["tool_calls"]), call)
			return
		}
	}
	message := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{call}}
	if len(*pendingReasoning) > 0 {
		message["reasoning_content"] = strings.Join(*pendingReasoning, "\n")
		*pendingReasoning = nil
	}
	*messages = append(*messages, message)
	*assistantIndex = len(*messages) - 1
}

func contentText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	var parts []string
	for _, raw := range anySlice(value) {
		block, _ := raw.(map[string]any)
		if text := stringValue(block["text"]); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

func responseOutputContent(value any, anthropic bool) any {
	if text, ok := value.(string); ok {
		return text
	}
	var blocks []any
	for _, raw := range anySlice(value) {
		part, _ := raw.(map[string]any)
		if part == nil {
			continue
		}
		switch stringValue(part["type"]) {
		case "input_text", "output_text", "text":
			blocks = append(blocks, map[string]any{"type": "text", "text": stringValue(part["text"])})
		case "input_image", "image_url":
			imageURL := stringValue(part["image_url"])
			if image, _ := part["image_url"].(map[string]any); imageURL == "" && image != nil {
				imageURL = stringValue(image["url"])
			}
			if imageURL == "" {
				continue
			}
			if anthropic {
				blocks = append(blocks, anthropicImageBlock(imageURL))
			} else {
				blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}})
			}
		}
	}
	if len(blocks) == 0 {
		encoded, err := json.Marshal(value)
		if err == nil {
			return string(encoded)
		}
		return ""
	}
	if len(blocks) == 1 {
		if block, _ := blocks[0].(map[string]any); block != nil && stringValue(block["type"]) == "text" {
			return stringValue(block["text"])
		}
	}
	return blocks
}

func lastUserText(input any) string {
	items := anySlice(input)
	for i := len(items) - 1; i >= 0; i-- {
		item, _ := items[i].(map[string]any)
		if stringValue(item["role"]) == "user" {
			return contentText(convertMessageContent(item["content"], false))
		}
	}
	if text, _ := input.(string); text != "" {
		return text
	}
	return ""
}

func anySlice(value any) []any {
	values, _ := value.([]any)
	return values
}

func stringValue(value any) string {
	s, _ := value.(string)
	return s
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func positiveInt(value any, fallback int) int {
	switch v := value.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil && n > 0 {
			return int(n)
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	case int:
		if v > 0 {
			return v
		}
	}
	return fallback
}

func copyIfPresent(dst, src map[string]any, keys ...string) {
	for _, key := range keys {
		if value, ok := src[key]; ok && value != nil {
			dst[key] = value
		}
	}
}

func valueOr(value, fallback any) any {
	if value == nil {
		return fallback
	}
	return value
}
