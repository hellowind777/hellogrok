package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

var errDeepSeekInsufficientSystemResource = errors.New("upstream inference stopped because DeepSeek had insufficient system resources")

const (
	deepSeekChatJSONInstructionPrefix = "Return exactly one valid JSON object."
	grokContextWindowHeader           = "X-Grok-Context-Window"
	grokMaxCompletionTokensHeader     = "X-Grok-Max-Completion-Tokens"
)

func isOfficialDeepSeekRoute(route config.Route) bool {
	return config.IsOfficialDeepSeekRoute(route)
}

func setGrokModelHeaders(header http.Header, route config.Route) {
	if route.ContextWindowConfigured {
		header.Set(grokContextWindowHeader, strconv.FormatUint(route.ContextWindow, 10))
	}
	if route.MaxCompletionTokensConfigured {
		header.Set(grokMaxCompletionTokensHeader, strconv.FormatUint(route.MaxCompletionTokens, 10))
	}
}

// mergeGrokModelHeaders keeps explicit model/provider config authoritative and
// otherwise accepts valid capacity metadata from any upstream channel.
func mergeGrokModelHeaders(header, upstream http.Header) {
	mergePositiveModelHeader(header, upstream, grokContextWindowHeader, 64)
	mergePositiveModelHeader(header, upstream, grokMaxCompletionTokensHeader, 32)
}

func mergePositiveModelHeader(header, upstream http.Header, name string, bitSize int) {
	if positiveModelHeader(header.Get(name), bitSize) {
		return
	}
	value := strings.TrimSpace(upstream.Get(name))
	if positiveModelHeader(value, bitSize) {
		header.Set(name, value)
	}
}

func positiveModelHeader(value string, bitSize int) bool {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, bitSize)
	return err == nil && parsed > 0
}

func normalizeDeepSeekRequest(root map[string]any, route config.Route, protocol wireProtocol) {
	if root == nil || !isOfficialDeepSeekRoute(route) {
		return
	}
	root["model"] = requestModelForRoute(route, protocol)
	switch protocol {
	case wireResponses:
		repairDeepSeekSearchHistory(root)
		normalizeDeepSeekResponsesRequest(root)
	case wireMessages:
		normalizeDeepSeekMessagesRequest(root, route.ReasoningEffortEnabled)
	case wireChatCompletions:
		normalizeDeepSeekChatRequest(root)
	}
}

func normalizeDeepSeekResponsesRequest(root map[string]any) {
	reasoning, _ := root["reasoning"].(map[string]any)
	if reasoning == nil {
		return
	}
	if effort := normalizedDeepSeekResponsesEffort(stringValue(reasoning["effort"])); effort != "" {
		reasoning["effort"] = effort
	}
}

func normalizeDeepSeekMessagesRequest(root map[string]any, reasoningSelectionEnabled bool) {
	thinking, _ := root["thinking"].(map[string]any)
	rawOutput, hasOutput := root["output_config"]
	output, outputIsObject := rawOutput.(map[string]any)
	if hasOutput && rawOutput != nil && !outputIsObject {
		// Preserve malformed caller input so DeepSeek reports the schema error;
		// silently deleting it would change the requested behavior.
		return
	}
	effort := ""
	if output != nil {
		effort = strings.ToLower(strings.TrimSpace(stringValue(output["effort"])))
	}
	if output != nil {
		// DeepSeek's Anthropic compatibility layer supports only effort inside
		// output_config. Grok Build uses its locally validated StructuredOutput
		// function for Messages schemas, so forwarding format would only ask the
		// provider to ignore a contract it cannot enforce.
		for key := range output {
			if key != "effort" {
				delete(output, key)
			}
		}
	}

	if effort == "none" {
		root["thinking"] = map[string]any{"type": "disabled"}
		delete(root, "output_config")
		return
	}
	// Grok Build serializes Messages effort=None by omitting both fields, while
	// DeepSeek interprets their absence as its default enabled/high mode. Managed
	// First-party models have an explicit High default, so an absent pair on this
	// private facade represents the user's None selection.
	if thinking == nil && effort == "" && reasoningSelectionEnabled {
		root["thinking"] = map[string]any{"type": "disabled"}
		delete(root, "output_config")
		return
	}
	if thinking != nil {
		typ := strings.ToLower(strings.TrimSpace(stringValue(thinking["type"])))
		if typ == "adaptive" {
			thinking["type"] = "enabled"
		} else if typ == "disabled" {
			delete(root, "output_config")
			return
		}
	}
	if normalized := normalizedDeepSeekChatMessagesEffort(effort); normalized != "" {
		output["effort"] = normalized
	} else if effort == "" {
		delete(root, "output_config")
	}
}

func normalizeDeepSeekChatRequest(root map[string]any) {
	if _, hasMaxTokens := root["max_tokens"]; !hasMaxTokens {
		if maxCompletionTokens, exists := root["max_completion_tokens"]; exists {
			root["max_tokens"] = maxCompletionTokens
		}
	}
	delete(root, "max_completion_tokens")

	if user, exists := root["user"]; exists {
		if current, present := root["user_id"]; !present || current == nil {
			root["user_id"] = user
		}
		delete(root, "user")
	}

	if stream, _ := root["stream"].(bool); stream {
		options, _ := root["stream_options"].(map[string]any)
		if options == nil {
			options = map[string]any{}
			root["stream_options"] = options
		}
		options["include_usage"] = true
	}

	thinking, _ := root["thinking"].(map[string]any)
	normalizeDeepSeekChatJSONOutput(root)
	effort := strings.ToLower(strings.TrimSpace(stringValue(root["reasoning_effort"])))
	switch {
	case effort == "none":
		root["thinking"] = map[string]any{"type": "disabled"}
		delete(root, "reasoning_effort")
	case thinking != nil && strings.EqualFold(stringValue(thinking["type"]), "disabled"):
		delete(root, "reasoning_effort")
	case effort != "":
		if normalized := normalizedDeepSeekChatMessagesEffort(effort); normalized != "" {
			root["reasoning_effort"] = normalized
		}
		if thinking == nil {
			thinking = map[string]any{}
			root["thinking"] = thinking
		}
		thinking["type"] = "enabled"
	case thinking != nil && strings.EqualFold(stringValue(thinking["type"]), "adaptive"):
		thinking["type"] = "enabled"
	}

	// DeepSeek's OpenAI-compatible thinking mode rejects tool_choice. An
	// omitted thinking object still means enabled, so remove the selector while
	// retaining declarations for the provider's default automatic tool use. A
	// disabled selector is represented by omitting both the selector and tools.
	rawThinking, hasThinking := root["thinking"]
	thinking, thinkingIsObject := rawThinking.(map[string]any)
	thinkingEnabled := !hasThinking || rawThinking == nil ||
		(thinkingIsObject && strings.EqualFold(stringValue(thinking["type"]), "enabled"))
	if thinkingEnabled {
		if toolChoiceDisablesTools(root["tool_choice"]) {
			delete(root, "tools")
		}
		delete(root, "tool_choice")
	}

	normalizeDeepSeekChatHistory(root)
}

func normalizeDeepSeekChatHistory(root map[string]any) {
	for _, raw := range anySlice(root["messages"]) {
		message, _ := raw.(map[string]any)
		if message == nil {
			continue
		}
		if strings.EqualFold(stringValue(message["role"]), "developer") {
			message["role"] = "system"
		}
		if strings.EqualFold(stringValue(message["role"]), "assistant") && len(anySlice(message["tool_calls"])) > 0 {
			if content, exists := message["content"]; !exists || content == nil {
				message["content"] = ""
			}
		}
	}
}

// DeepSeek Chat supports JSON object mode but not OpenAI's json_schema response
// format. Keep Grok Build's structured-output contract by putting the schema in
// a system instruction; Build still validates the returned object locally.
func normalizeDeepSeekChatJSONOutput(root map[string]any) {
	format, _ := root["response_format"].(map[string]any)
	typ := strings.ToLower(strings.TrimSpace(stringValue(format["type"])))
	if typ != "json_object" && typ != "json_schema" {
		return
	}

	instruction := deepSeekChatJSONInstructionPrefix + " Do not emit markdown or text outside the JSON object."
	if typ == "json_schema" {
		definition, _ := format["json_schema"].(map[string]any)
		schema, exists := definition["schema"]
		if definition == nil || !exists || schema == nil {
			// Leave malformed input intact so the provider reports the request error
			// instead of silently weakening an unverifiable schema contract.
			return
		}
		encoded, err := json.Marshal(schema)
		if err != nil {
			return
		}
		instruction += " Match this JSON Schema exactly:\n" + string(encoded)
		root["response_format"] = map[string]any{"type": "json_object"}
	}

	messages := anySlice(root["messages"])
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		role := strings.ToLower(strings.TrimSpace(stringValue(message["role"])))
		if (role == "system" || role == "developer") &&
			strings.Contains(stringValue(message["content"]), deepSeekChatJSONInstructionPrefix) {
			return
		}
	}
	root["messages"] = append([]any{map[string]any{"role": "system", "content": instruction}}, messages...)
}

func normalizedDeepSeekResponsesEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none":
		return "none"
	case "minimal", "low":
		return "low"
	case "medium", "high", "xhigh":
		return "high"
	case "max":
		return "max"
	}
	return ""
}

func normalizedDeepSeekChatMessagesEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low":
		return "low"
	case "medium", "high", "xhigh":
		return "high"
	case "max":
		return "max"
	}
	return ""
}

// requestModelForRoute keeps provider model IDs data-driven. The documented
// [1m] suffix is an Anthropic Messages alias, so protocol bridges strip only
// that suffix without making assumptions about the underlying model name.
func requestModelForRoute(route config.Route, protocol wireProtocol) string {
	model, alias := splitDeepSeekMessagesAlias(route.WireModel)
	if protocol == wireMessages && alias {
		return model + "[1m]"
	}
	return model
}

// responseModelForRoute is the model ID DeepSeek reports in response objects.
// Anthropic's [1m] request alias resolves to the underlying model.
func responseModelForRoute(route config.Route) string {
	if !isOfficialDeepSeekRoute(route) {
		return route.WireModel
	}
	model, _ := splitDeepSeekMessagesAlias(route.WireModel)
	return model
}

func splitDeepSeekMessagesAlias(value string) (string, bool) {
	model := strings.TrimSpace(value)
	const suffix = "[1m]"
	if len(model) >= len(suffix) && strings.EqualFold(model[len(model)-len(suffix):], suffix) {
		return strings.TrimSpace(model[:len(model)-len(suffix)]), true
	}
	return model, false
}

func deepSeekChatInsufficientSystemResource(root map[string]any, route config.Route, protocol wireProtocol) bool {
	if protocol != wireChatCompletions || !isOfficialDeepSeekRoute(route) {
		return false
	}
	for _, raw := range anySlice(root["choices"]) {
		choice, _ := raw.(map[string]any)
		if strings.EqualFold(strings.TrimSpace(stringValue(choice["finish_reason"])), "insufficient_system_resource") {
			return true
		}
	}
	return false
}

func (s *Server) upstreamForRoute(route config.Route) (*http.Client, time.Duration, time.Duration) {
	if isOfficialDeepSeekRoute(route) {
		timeout := routeUpstreamIdleTimeout(route, defaultDeepSeekResponseHeaderTimeout)
		return s.deepSeekClient, timeout, routeUpstreamIdleTimeout(route, s.deepSeekBodyIdleTimeout)
	}
	timeout := routeUpstreamIdleTimeout(route, defaultUpstreamResponseHeaderTimeout)
	return s.client, timeout, routeUpstreamIdleTimeout(route, s.bodyIdleTimeout)
}
