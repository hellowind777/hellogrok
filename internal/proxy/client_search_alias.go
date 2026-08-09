package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

const clientWebSearchWireAliasBase = "hellogrok_web_search"

// chooseClientWebSearchWireAlias keeps Build's public web_search name away
// from upstreams that reserve or intercept that name as a hosted tool.
func chooseClientWebSearchWireAlias(root map[string]any) string {
	used := map[string]struct{}{}
	hasClientSearch := false
	for _, raw := range anySlice(root["tools"]) {
		tool, _ := raw.(map[string]any)
		name := strings.ToLower(strings.TrimSpace(functionToolName(tool)))
		if name == "" {
			continue
		}
		used[name] = struct{}{}
		if name == "web_search" {
			hasClientSearch = true
		}
	}
	if !hasClientSearch {
		return ""
	}
	for suffix := 0; ; suffix++ {
		candidate := clientWebSearchWireAliasBase
		if suffix > 0 {
			candidate = fmt.Sprintf("%s_%d", clientWebSearchWireAliasBase, suffix+1)
		}
		if _, exists := used[candidate]; !exists {
			return candidate
		}
	}
}

// aliasClientWebSearchOnWire rewrites only protocol-defined request fields.
// Tool arguments and results are business data and must never be traversed.
func aliasClientWebSearchOnWire(root map[string]any, alias string, protocol wireProtocol) bool {
	if strings.TrimSpace(alias) == "" {
		return false
	}
	return rewriteClientWebSearchRequest(root, "web_search", alias, protocol)
}

func restoreClientWebSearchAlias(root map[string]any, alias string, protocol wireProtocol) bool {
	if strings.TrimSpace(alias) == "" {
		return false
	}
	return rewriteClientWebSearchResponse(root, alias, "web_search", protocol)
}

func restoreClientWebSearchAliasJSON(data []byte, alias string, protocol wireProtocol) ([]byte, error) {
	root, err := decodeJSONMap(data)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(alias) == "" || !restoreClientWebSearchAlias(root, alias, protocol) {
		return data, nil
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

func rewriteClientWebSearchRequest(root map[string]any, from, to string, protocol wireProtocol) bool {
	switch protocol {
	case wireResponses:
		changed := rewriteResponsesToolDeclarations(root["tools"], from, to)
		changed = rewriteResponsesToolChoice(root["tool_choice"], from, to) || changed
		changed = rewriteResponsesItems(root["input"], from, to) || changed
		return changed
	case wireMessages:
		changed := rewriteMessagesToolDeclarations(root["tools"], from, to)
		changed = rewriteMessagesToolChoice(root["tool_choice"], from, to) || changed
		for _, raw := range anySlice(root["messages"]) {
			message, _ := raw.(map[string]any)
			changed = rewriteMessagesContent(message["content"], from, to) || changed
		}
		return changed
	case wireChatCompletions:
		changed := rewriteChatToolDeclarations(root["tools"], from, to)
		changed = rewriteChatToolChoice(root["tool_choice"], from, to) || changed
		for _, raw := range anySlice(root["messages"]) {
			message, _ := raw.(map[string]any)
			changed = rewriteChatMessageToolCalls(message, from, to) || changed
		}
		return changed
	default:
		return false
	}
}

func rewriteClientWebSearchResponse(root map[string]any, from, to string, protocol wireProtocol) bool {
	switch protocol {
	case wireResponses:
		changed := rewriteResponsesItems(root["output"], from, to)
		if item, _ := root["item"].(map[string]any); item != nil {
			changed = rewriteResponsesItem(item, from, to) || changed
		}
		if response, _ := root["response"].(map[string]any); response != nil {
			changed = rewriteResponsesItems(response["output"], from, to) || changed
		}
		return changed
	case wireMessages:
		changed := rewriteMessagesContent(root["content"], from, to)
		if message, _ := root["message"].(map[string]any); message != nil {
			changed = rewriteMessagesContent(message["content"], from, to) || changed
		}
		if block, _ := root["content_block"].(map[string]any); block != nil {
			changed = rewriteMessagesToolUse(block, from, to) || changed
		}
		return changed
	case wireChatCompletions:
		changed := false
		for _, raw := range anySlice(root["choices"]) {
			choice, _ := raw.(map[string]any)
			for _, key := range []string{"message", "delta"} {
				message, _ := choice[key].(map[string]any)
				changed = rewriteChatMessageToolCalls(message, from, to) || changed
			}
		}
		return changed
	default:
		return false
	}
}

func rewriteResponsesToolDeclarations(value any, from, to string) bool {
	changed := false
	for _, raw := range anySlice(value) {
		tool, _ := raw.(map[string]any)
		if strings.EqualFold(strings.TrimSpace(stringValue(tool["type"])), "function") {
			changed = rewriteFunctionName(tool, from, to) || changed
		}
	}
	return changed
}

func rewriteResponsesToolChoice(value any, from, to string) bool {
	choice, _ := value.(map[string]any)
	if choice == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(stringValue(choice["type"]))) {
	case "function":
		return rewriteFunctionName(choice, from, to)
	case "allowed_tools":
		return rewriteResponsesToolDeclarations(choice["tools"], from, to)
	default:
		return false
	}
}

func rewriteResponsesItems(value any, from, to string) bool {
	changed := false
	for _, raw := range anySlice(value) {
		item, _ := raw.(map[string]any)
		changed = rewriteResponsesItem(item, from, to) || changed
	}
	return changed
}

func rewriteResponsesItem(item map[string]any, from, to string) bool {
	if item == nil || !strings.EqualFold(strings.TrimSpace(stringValue(item["type"])), "function_call") {
		return false
	}
	return rewriteName(item, from, to)
}

func rewriteMessagesToolDeclarations(value any, from, to string) bool {
	changed := false
	for _, raw := range anySlice(value) {
		tool, _ := raw.(map[string]any)
		if tool == nil || tool["input_schema"] == nil {
			continue
		}
		changed = rewriteName(tool, from, to) || changed
	}
	return changed
}

func rewriteMessagesToolChoice(value any, from, to string) bool {
	choice, _ := value.(map[string]any)
	if choice == nil || !strings.EqualFold(strings.TrimSpace(stringValue(choice["type"])), "tool") {
		return false
	}
	return rewriteName(choice, from, to)
}

func rewriteMessagesContent(value any, from, to string) bool {
	changed := false
	for _, raw := range anySlice(value) {
		block, _ := raw.(map[string]any)
		changed = rewriteMessagesToolUse(block, from, to) || changed
	}
	return changed
}

func rewriteMessagesToolUse(block map[string]any, from, to string) bool {
	if block == nil || !strings.EqualFold(strings.TrimSpace(stringValue(block["type"])), "tool_use") {
		return false
	}
	return rewriteName(block, from, to)
}

func rewriteChatToolDeclarations(value any, from, to string) bool {
	changed := false
	for _, raw := range anySlice(value) {
		tool, _ := raw.(map[string]any)
		if strings.EqualFold(strings.TrimSpace(stringValue(tool["type"])), "function") {
			changed = rewriteFunctionName(tool, from, to) || changed
		}
	}
	return changed
}

func rewriteChatToolChoice(value any, from, to string) bool {
	choice, _ := value.(map[string]any)
	if choice == nil || !strings.EqualFold(strings.TrimSpace(stringValue(choice["type"])), "function") {
		return false
	}
	return rewriteFunctionName(choice, from, to)
}

func rewriteChatMessageToolCalls(message map[string]any, from, to string) bool {
	if message == nil {
		return false
	}
	changed := false
	for _, raw := range anySlice(message["tool_calls"]) {
		call, _ := raw.(map[string]any)
		if strings.EqualFold(strings.TrimSpace(stringValue(call["type"])), "function") {
			changed = rewriteFunctionName(call, from, to) || changed
		}
	}
	if call, _ := message["function_call"].(map[string]any); call != nil {
		changed = rewriteName(call, from, to) || changed
	}
	return changed
}

func rewriteFunctionName(container map[string]any, from, to string) bool {
	changed := rewriteName(container, from, to)
	if function, _ := container["function"].(map[string]any); function != nil {
		changed = rewriteName(function, from, to) || changed
	}
	return changed
}

func rewriteName(container map[string]any, from, to string) bool {
	if container == nil || !strings.EqualFold(strings.TrimSpace(stringValue(container["name"])), from) {
		return false
	}
	container["name"] = to
	return true
}

func isClientWebSearchWireAlias(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return name == clientWebSearchWireAliasBase || strings.HasPrefix(name, clientWebSearchWireAliasBase+"_")
}
