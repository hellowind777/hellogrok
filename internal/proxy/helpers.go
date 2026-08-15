package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hellowind777/hellogrok/internal/appinfo"
)

type requestMeta struct {
	Timestamp            string `json:"timestamp"`
	Target               string `json:"target"`
	Model                string `json:"model"`
	Bytes                int    `json:"bytes"`
	Tools                int    `json:"tools"`
	WebSearch            int    `json:"web_search"`
	HostedWebSearch      int    `json:"hosted_web_search"`
	FunctionWebSearch    int    `json:"function_web_search"`
	XSearch              int    `json:"x_search"`
	BuildHostedWebSearch int    `json:"build_hosted_web_search"`
	BuildXSearch         int    `json:"build_x_search"`
	ProxyAddedWebSearch  bool   `json:"proxy_added_web_search"`
	ClientSearchPrepared bool   `json:"client_web_search_prepared"`
	ClientSearchAliased  bool   `json:"client_web_search_aliased"`
	OpaqueReasoning      int    `json:"opaque_reasoning"`
	ReasoningDropped     int    `json:"reasoning_dropped"`
	ReasoningUnknown     int    `json:"reasoning_unknown"`
	ReasoningRecovery    bool   `json:"reasoning_recovery"`
}

var requestMetaWriteMu sync.Mutex

type wireProtocol string

const (
	wireUnknown         wireProtocol = "unknown"
	wireResponses       wireProtocol = "responses"
	wireMessages        wireProtocol = "messages"
	wireChatCompletions wireProtocol = "chat_completions"
)

// saveLastRequestMeta persists only structural diagnostics. Request content,
// tool descriptions, credentials, and user prompts are never written.
func saveLastRequestMeta(target, model string, bodyBytes, tools, webSearch, hostedWebSearch, functionWebSearch, xSearch int, request facadeRequest) {
	requestMetaWriteMu.Lock()
	defer requestMetaWriteMu.Unlock()

	dir := appinfo.DataDir()
	_ = os.MkdirAll(dir, 0o700)
	purgeLegacyRequestDiagnostics()
	b, err := json.Marshal(requestMeta{
		Timestamp:            time.Now().UTC().Format(time.RFC3339Nano),
		Target:               safeDiagnosticTarget(target),
		Model:                model,
		Bytes:                bodyBytes,
		Tools:                tools,
		WebSearch:            webSearch,
		HostedWebSearch:      hostedWebSearch,
		FunctionWebSearch:    functionWebSearch,
		XSearch:              xSearch,
		BuildHostedWebSearch: request.BuildHostedWebSearch,
		BuildXSearch:         request.BuildXSearch,
		ProxyAddedWebSearch:  request.ProxyAddedWebSearch,
		ClientSearchPrepared: request.ClientSearchPrepared,
		ClientSearchAliased:  request.ClientSearchAlias != "",
		OpaqueReasoning:      request.Reasoning.Opaque,
		ReasoningDropped:     request.Reasoning.Dropped,
		ReasoningUnknown:     request.Reasoning.Unknown,
		ReasoningRecovery:    request.ReasoningRecovery,
	})
	if err == nil {
		_ = writeRequestMetaAtomic(filepath.Join(dir, "last_request_meta.json"), b)
	}
}

func writeRequestMetaAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".hellogrok-request-meta-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	committed = true
	return nil
}

func purgeLegacyRequestDiagnostics() {
	dir := appinfo.DataDir()
	_ = os.Remove(filepath.Join(dir, "last_request.json"))
	_ = os.Remove(filepath.Join(dir, "last_request_meta.txt"))
}

// summarizeBody reports only tool structure needed for compatibility diagnostics.
func summarizeBody(body []byte) (tools, webSearch, hostedWebSearch, functionWebSearch, xSearch int) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return 0, 0, 0, 0, 0
	}
	definitions, _ := m["tools"].([]any)
	tools = len(definitions)
	for _, field := range []string{"web_search_options", "search_parameters"} {
		if options, exists := m[field]; exists && options != nil {
			tools++
			webSearch++
			hostedWebSearch++
		}
	}
	for _, definition := range definitions {
		tool, ok := definition.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := tool["type"].(string)
		if typ == "x_search" {
			xSearch++
		}
		name, _ := tool["name"].(string)
		if fn, ok := tool["function"].(map[string]any); ok && name == "" {
			name, _ = fn["name"].(string)
		}
		if typ == "web_search" || strings.HasPrefix(typ, "web_search_") {
			hostedWebSearch++
			webSearch++
		} else if name == "web_search" || isClientWebSearchWireAlias(name) {
			functionWebSearch++
			webSearch++
		}
	}
	return tools, webSearch, hostedWebSearch, functionWebSearch, xSearch
}

func decodeRequestObject(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, fmt.Errorf("request body contains trailing JSON")
	}
	return root, nil
}

func encodeRequestObject(root map[string]any) ([]byte, error) {
	out, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func toolChoiceDisablesTools(choice any) bool {
	switch value := choice.(type) {
	case string:
		return value == "none"
	case map[string]any:
		typ, _ := value["type"].(string)
		return typ == "none"
	default:
		return false
	}
}

type hostedSearchCapabilities struct {
	Web bool
	X   bool
}

func (c hostedSearchCapabilities) any() bool {
	return c.Web || c.X
}

// normalizeHostedSearchRequest preserves the historical wrapper used by the
// focused normalization tests. Runtime routing uses the capability-aware
// variant below.
func normalizeHostedSearchRequest(body []byte, grokRoute bool) ([]byte, bool, error) {
	return normalizeHostedSearchRequestForCapabilities(body, hostedSearchCapabilities{
		Web: true,
		X:   grokRoute,
	})
}

// normalizeHostedSearchRequestForCapabilities emits only the hosted-search
// declarations confirmed for the selected upstream. Ordinary function tools
// remain untouched when no hosted search capability is available.
func normalizeHostedSearchRequestForCapabilities(body []byte, capabilities hostedSearchCapabilities) ([]byte, bool, error) {
	root, err := decodeRequestObject(body)
	if err != nil {
		return body, false, err
	}
	changed := normalizeHostedSearchObject(root, capabilities)
	if !changed {
		return body, false, nil
	}
	out, err := encodeRequestObject(root)
	if err != nil {
		return body, false, err
	}
	return out, true, nil
}

func normalizeHostedSearchObject(root map[string]any, capabilities hostedSearchCapabilities) bool {
	changed := false

	tools, _ := root["tools"].([]any)
	hasHosted := false
	var canonicalWeb map[string]any
	var canonicalX map[string]any
	for _, entry := range tools {
		tool, _ := entry.(map[string]any)
		typ := stringValue(tool["type"])
		if isHostedWebSearchType(typ) {
			hasHosted = true
			if canonicalWeb == nil {
				canonicalWeb = cloneMap(tool)
				if stringValue(canonicalWeb["type"]) != "web_search" {
					canonicalWeb["type"] = "web_search"
					changed = true
				}
			}
		}
		if typ == "x_search" {
			hasHosted = true
			if canonicalX == nil {
				canonicalX = cloneMap(tool)
			}
		}
	}
	if canonicalWeb == nil && hasHosted && capabilities.Web {
		canonicalWeb = map[string]any{"type": "web_search"}
		changed = true
	}
	if canonicalX == nil && hasHosted && capabilities.X {
		canonicalX = map[string]any{"type": "x_search"}
		changed = true
	}
	normalized := make([]any, 0, len(tools))
	hostedInserted := false
	for _, entry := range tools {
		tool, _ := entry.(map[string]any)
		typ := stringValue(tool["type"])
		if isHostedWebSearchType(typ) || typ == "x_search" {
			if !hostedInserted {
				if capabilities.Web {
					normalized = append(normalized, canonicalWeb)
				}
				if capabilities.X {
					normalized = append(normalized, canonicalX)
				}
				hostedInserted = true
			}
			continue
		}
		if hasHosted && capabilities.any() && isSearchFunctionTool(tool) {
			changed = true
			continue
		}
		normalized = append(normalized, entry)
	}
	if hasHosted {
		if !hostedInserted {
			if capabilities.Web {
				normalized = append(normalized, canonicalWeb)
			}
			if capabilities.X {
				normalized = append(normalized, canonicalX)
			}
		}
		if !sameJSONValue(tools, normalized) {
			root["tools"] = normalized
			changed = true
		}
	}
	if hasHosted && normalizeHostedToolChoice(root, capabilities) {
		changed = true
	}
	return changed
}

// DeepSeek Responses returns a non-standard action.queries field and requires
// it when the completed web_search_call is replayed on the next turn. Build's
// standard Responses model drops that field while deserializing the response.
// The caller must scope this repair to the first-party Responses endpoint; the
// payload alone cannot distinguish DeepSeek from a relay reusing its model ID.
func repairDeepSeekSearchHistory(root map[string]any) bool {
	input, ok := root["input"].([]any)
	if !ok {
		return false
	}

	changed := false
	for _, entry := range input {
		item, _ := entry.(map[string]any)
		if typ, _ := item["type"].(string); typ != "web_search_call" {
			continue
		}
		action, _ := item["action"].(map[string]any)
		if typ, _ := action["type"].(string); typ != "search" {
			continue
		}
		if queries, exists := action["queries"]; exists && queries != nil {
			continue
		}
		query, _ := action["query"].(string)
		if query = strings.TrimSpace(query); query != "" {
			action["queries"] = []any{query}
		} else {
			action["queries"] = []any{}
		}
		changed = true
	}
	return changed
}

func isHostedWebSearchType(typ string) bool {
	typ = strings.ToLower(strings.TrimSpace(typ))
	return typ == "web_search" || strings.HasPrefix(typ, "web_search_")
}

func functionToolName(tool map[string]any) string {
	if tool == nil {
		return ""
	}
	typ := stringValue(tool["type"])
	_, messagesFunction := tool["input_schema"]
	if typ != "function" && !messagesFunction {
		return ""
	}
	if name := stringValue(tool["name"]); name != "" {
		return name
	}
	function, _ := tool["function"].(map[string]any)
	return stringValue(function["name"])
}

func isSearchFunctionTool(tool map[string]any) bool {
	name := strings.ToLower(strings.TrimSpace(functionToolName(tool)))
	return name == "web_search" || name == "x_search"
}

func normalizeHostedToolChoice(root map[string]any, capabilities hostedSearchCapabilities) bool {
	choice := root["tool_choice"]
	if choice == "required" && !requestHasCallableTools(root) {
		delete(root, "tool_choice")
		return true
	}
	value, ok := choice.(map[string]any)
	if !ok {
		return false
	}
	typ := strings.ToLower(strings.TrimSpace(stringValue(value["type"])))
	if typ == "allowed_tools" {
		changed, empty := normalizeAllowedHostedTools(value, capabilities)
		if empty {
			delete(root, "tool_choice")
			return true
		}
		return changed
	}
	name := strings.ToLower(strings.TrimSpace(stringValue(value["name"])))
	if function, _ := value["function"].(map[string]any); name == "" && function != nil {
		name = strings.ToLower(strings.TrimSpace(stringValue(function["name"])))
	}
	if typ == "web_search" && capabilities.Web {
		return false
	}
	if typ == "x_search" && capabilities.X {
		return false
	}
	isHostedChoice := typ == "x_search" || isHostedWebSearchType(typ)
	isCollidingFunction := typ == "function" && (name == "web_search" || name == "x_search")
	if !isHostedChoice && !(capabilities.any() && isCollidingFunction) {
		return false
	}
	if !capabilities.any() {
		delete(root, "tool_choice")
		return true
	}
	for key := range value {
		delete(value, key)
	}
	preferX := typ == "x_search" || name == "x_search"
	if preferX && capabilities.X {
		value["type"] = "x_search"
	} else if capabilities.Web {
		value["type"] = "web_search"
	} else {
		value["type"] = "x_search"
	}
	return true
}

func normalizeAllowedHostedTools(choice map[string]any, capabilities hostedSearchCapabilities) (changed, empty bool) {
	allowed, ok := choice["tools"].([]any)
	if !ok {
		return false, false
	}
	var normalized []any
	searchSeen := false
	for _, raw := range allowed {
		tool, _ := raw.(map[string]any)
		typ := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		name := strings.ToLower(strings.TrimSpace(functionToolName(tool)))
		isHosted := typ == "x_search" || isHostedWebSearchType(typ)
		isCollidingFunction := typ == "function" && (name == "web_search" || name == "x_search")
		if !isHosted && !(capabilities.any() && isCollidingFunction) {
			normalized = append(normalized, raw)
			continue
		}
		if searchSeen {
			continue
		}
		searchSeen = true
		if capabilities.Web {
			normalized = append(normalized, map[string]any{"type": "web_search"})
		}
		if capabilities.X {
			normalized = append(normalized, map[string]any{"type": "x_search"})
		}
	}
	if !searchSeen || sameJSONValue(allowed, normalized) {
		return false, len(normalized) == 0
	}
	choice["tools"] = normalized
	return true, len(normalized) == 0
}

func requestHasCallableTools(root map[string]any) bool {
	for _, raw := range anySlice(root["tools"]) {
		tool, _ := raw.(map[string]any)
		if tool == nil {
			continue
		}
		typ := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		if typ == "function" || typ == "x_search" || isHostedWebSearchType(typ) {
			return true
		}
	}
	return false
}

func sameJSONValue(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}
