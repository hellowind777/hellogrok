package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

const maxBackfilledSearchSources = 100

var httpURLInTextPattern = regexp.MustCompile(`https?://[^\s<>"'()\[\]{}]+`)

type canonicalResult struct {
	Output           []any
	InputTokens      int
	OutputTokens     int
	CachedTokens     int
	ReasoningTokens  int
	IncompleteReason string
}

func canonicalFromMessages(data []byte, hosted bool, query string) (canonicalResult, error) {
	root, err := decodeJSONMap(data)
	if err != nil {
		return canonicalResult{}, err
	}
	validationRoot := root
	if hosted {
		validationRoot = cloneMap(root)
		stripMessagesHostedSearchBlocks(validationRoot)
	}
	if err := validateMessagesEnvelope(validationRoot); err != nil {
		return canonicalResult{}, err
	}
	var result canonicalResult
	webCalls := map[string]map[string]any{}
	var textParts []string
	var annotations []any
	var evidenceURLs []string
	flushText := func() {
		if len(textParts) == 0 {
			return
		}
		result.Output = append(result.Output, messageItem(strings.Join(textParts, "\n"), annotations))
		textParts = nil
		annotations = nil
	}
	for _, raw := range anySlice(root["content"]) {
		block, _ := raw.(map[string]any)
		typ := stringValue(block["type"])
		if typ != "text" {
			flushText()
		}
		switch typ {
		case "thinking":
			if text := stringValue(block["thinking"]); text != "" {
				result.Output = append(result.Output, reasoningItem(text, stringValue(block["signature"])))
			}
		case "redacted_thinking":
			result.Output = append(result.Output, reasoningItem("", stringValue(block["data"])))
		case "server_tool_use":
			if stringValue(block["name"]) != "web_search" {
				continue
			}
			input, _ := block["input"].(map[string]any)
			query := firstString(input, "query", "q")
			item := webSearchItem(firstString(block, "id"), query, nil, "completed")
			result.Output = append(result.Output, item)
			webCalls[firstString(block, "id")] = item
		case "web_search_tool_result":
			call := webCalls[firstString(block, "tool_use_id")]
			if call == nil {
				call = webSearchItem(firstString(block, "tool_use_id"), "", nil, "completed")
				result.Output = append(result.Output, call)
			}
			sources, failed := messageSearchSources(block["content"])
			action, _ := call["action"].(map[string]any)
			action["sources"] = sources
			if failed {
				call["status"] = "failed"
			}
		case "tool_use":
			args, _ := json.Marshal(valueOr(block["input"], map[string]any{}))
			result.Output = append(result.Output, functionCallItem(firstString(block, "id"), stringValue(block["name"]), string(args)))
		case "text":
			textParts = append(textParts, stringValue(block["text"]))
			annotations = append(annotations, citationsToAnnotations(block["citations"])...)
			evidenceURLs = mergeUniqueStrings(evidenceURLs, urlsFromJSON(block["citations"])...)
		}
	}
	flushText()
	if usage, _ := root["usage"].(map[string]any); usage != nil {
		result.InputTokens = numberInt(usage["input_tokens"])
		result.CachedTokens = numberInt(usage["cache_read_input_tokens"])
		result.OutputTokens = numberInt(usage["output_tokens"])
	}
	if stringValue(root["stop_reason"]) == "max_tokens" {
		result.IncompleteReason = "max_output_tokens"
	}
	if hosted && len(webCalls) == 0 && (len(evidenceURLs) > 0 || positiveSearchUsage(root["usage"])) {
		search := webSearchItem("", query, urlsToSources(evidenceURLs), "completed")
		insertAt := len(result.Output)
		for index, raw := range result.Output {
			item, _ := raw.(map[string]any)
			if stringValue(item["type"]) == "message" {
				insertAt = index
				break
			}
		}
		result.Output = append(result.Output, nil)
		copy(result.Output[insertAt+1:], result.Output[insertAt:])
		result.Output[insertAt] = search
	}
	return result, nil
}

func canonicalFromChat(data []byte, hosted bool, query string) (canonicalResult, error) {
	root, err := decodeJSONMap(data)
	if err != nil {
		return canonicalResult{}, err
	}
	if err := validateChatEnvelope(root); err != nil {
		return canonicalResult{}, err
	}
	var result canonicalResult
	choices, ok := root["choices"].([]any)
	if !ok || len(choices) == 0 {
		return result, fmt.Errorf("chat completions response has no choices")
	}
	choice, ok := choices[0].(map[string]any)
	if !ok || choice == nil {
		return result, fmt.Errorf("chat completions response choice[0] must be an object")
	}
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return result, fmt.Errorf("chat completions response has no message")
	}
	if reasoning := firstString(message, "reasoning_content", "reasoning"); reasoning != "" {
		result.Output = append(result.Output, reasoningItem(reasoning, ""))
	}
	urls := collectCitationURLs(root, choice, message)
	if hosted && chatSearchExecuted(root, choice, message, urls) {
		result.Output = append(result.Output, webSearchItem("", query, urlsToSources(urls), "completed"))
	}
	for _, raw := range anySlice(message["tool_calls"]) {
		call, _ := raw.(map[string]any)
		fn, _ := call["function"].(map[string]any)
		args := stringValue(fn["arguments"])
		if args == "" && fn["arguments"] != nil {
			encoded, _ := json.Marshal(fn["arguments"])
			args = string(encoded)
		}
		result.Output = append(result.Output, functionCallItem(firstString(call, "id"), stringValue(fn["name"]), args))
	}
	if text := chatMessageText(message["content"]); text != "" {
		result.Output = append(result.Output, messageItem(text, urlsToAnnotations(urls)))
	}
	if usage, _ := root["usage"].(map[string]any); usage != nil {
		result.InputTokens = numberInt(valueFirst(usage, "prompt_tokens", "input_tokens"))
		result.OutputTokens = numberInt(valueFirst(usage, "completion_tokens", "output_tokens"))
		if details, _ := usage["prompt_tokens_details"].(map[string]any); details != nil {
			result.CachedTokens = numberInt(details["cached_tokens"])
		}
		if details, _ := usage["completion_tokens_details"].(map[string]any); details != nil {
			result.ReasoningTokens = numberInt(details["reasoning_tokens"])
		}
	}
	if finish := stringValue(choice["finish_reason"]); finish == "length" {
		result.IncompleteReason = "max_output_tokens"
	}
	return result, nil
}

func validateResponsesEnvelope(root map[string]any) error {
	if strings.TrimSpace(stringValue(root["id"])) == "" {
		return fmt.Errorf("Responses response id must be a non-empty string")
	}
	if stringValue(root["object"]) != "response" {
		return fmt.Errorf("Responses response object must be %q", "response")
	}
	if strings.TrimSpace(stringValue(root["status"])) == "" {
		return fmt.Errorf("Responses response status must be a non-empty string")
	}
	output, ok := root["output"].([]any)
	if !ok {
		return fmt.Errorf("Responses response output must be an array")
	}
	for index, raw := range output {
		item, ok := raw.(map[string]any)
		if !ok || item == nil {
			return fmt.Errorf("Responses response output[%d] must be an object", index)
		}
		switch stringValue(item["type"]) {
		case "message":
			content, ok := item["content"].([]any)
			if !ok {
				return fmt.Errorf("Responses response output[%d].content must be an array", index)
			}
			for contentIndex, rawPart := range content {
				if part, ok := rawPart.(map[string]any); !ok || part == nil {
					return fmt.Errorf("Responses response output[%d].content[%d] must be an object", index, contentIndex)
				}
			}
		case "reasoning":
			rawContent, exists := item["content"]
			if !exists || rawContent == nil {
				continue
			}
			content, ok := rawContent.([]any)
			if !ok {
				return fmt.Errorf("Responses response output[%d].content must be an array when present", index)
			}
			for contentIndex, rawPart := range content {
				if part, ok := rawPart.(map[string]any); !ok || part == nil {
					return fmt.Errorf("Responses response output[%d].content[%d] must be an object", index, contentIndex)
				}
			}
		}
	}
	return nil
}

func validateMessagesEnvelope(root map[string]any) error {
	for _, field := range []string{"id", "model"} {
		if strings.TrimSpace(stringValue(root[field])) == "" {
			return fmt.Errorf("Messages response %s must be a non-empty string", field)
		}
	}
	if stringValue(root["type"]) != "message" {
		return fmt.Errorf("Messages response type must be %q", "message")
	}
	if stringValue(root["role"]) != "assistant" {
		return fmt.Errorf("Messages response role must be %q", "assistant")
	}
	content, ok := root["content"].([]any)
	if !ok {
		return fmt.Errorf("Messages response content must be an array")
	}
	for index, raw := range content {
		block, ok := raw.(map[string]any)
		if !ok || block == nil {
			return fmt.Errorf("Messages response content[%d] must be an object", index)
		}
		if err := validateMessagesContentBlock(block); err != nil {
			return fmt.Errorf("Messages response content[%d]: %w", index, err)
		}
	}
	if usage, ok := root["usage"].(map[string]any); !ok || usage == nil {
		return fmt.Errorf("Messages response usage must be an object")
	}
	return nil
}

// Some Messages-compatible gateways omit empty placeholders from streamed
// block starts and send the real values later as deltas. Grok Build's Messages
// consumer intentionally models those start fields as required.
func normalizeMessagesStreamRequiredFields(root map[string]any) {
	switch stringValue(root["type"]) {
	case "content_block_start":
		if block, _ := root["content_block"].(map[string]any); block != nil {
			normalizeMessagesStreamBlock(block)
		}
	case "content_block_delta":
		if delta, _ := root["delta"].(map[string]any); delta != nil {
			switch stringValue(delta["type"]) {
			case "text_delta":
				setMissingString(delta, "text")
			case "input_json_delta":
				setMissingString(delta, "partial_json")
			case "thinking_delta":
				setMissingString(delta, "thinking")
			case "signature_delta":
				setMissingString(delta, "signature")
			}
		}
	}
}

func normalizeMessagesStreamBlock(block map[string]any) {
	switch stringValue(block["type"]) {
	case "text":
		setMissingString(block, "text")
	case "tool_use":
		if input, exists := block["input"]; !exists || input == nil {
			block["input"] = map[string]any{}
		}
	case "thinking":
		setMissingString(block, "thinking")
		setMissingString(block, "signature")
	}
}

func setMissingString(value map[string]any, key string) {
	if current, exists := value[key]; !exists || current == nil {
		value[key] = ""
	}
}

func validateMessagesContentBlock(block map[string]any) error {
	switch stringValue(block["type"]) {
	case "text":
		if _, ok := block["text"].(string); !ok {
			return fmt.Errorf("text block text must be a string")
		}
	case "tool_use":
		if strings.TrimSpace(stringValue(block["id"])) == "" {
			return fmt.Errorf("tool_use block id must be a non-empty string")
		}
		if strings.TrimSpace(stringValue(block["name"])) == "" {
			return fmt.Errorf("tool_use block name must be a non-empty string")
		}
		if _, exists := block["input"]; !exists {
			return fmt.Errorf("tool_use block input is required")
		}
	case "thinking":
		if _, ok := block["thinking"].(string); !ok {
			return fmt.Errorf("thinking block thinking must be a string")
		}
		if _, ok := block["signature"].(string); !ok {
			return fmt.Errorf("thinking block signature must be a string")
		}
	case "redacted_thinking":
		if _, ok := block["data"].(string); !ok {
			return fmt.Errorf("redacted_thinking block data must be a string")
		}
	case "image", "tool_result":
		// Both variants are part of Grok Build's current Messages ContentBlock
		// enum, although they are unusual in an assistant response.
	case "":
		return fmt.Errorf("content block type must be a non-empty string")
	default:
		return fmt.Errorf("unsupported content block type %q", stringValue(block["type"]))
	}
	return nil
}

func validateMessagesStreamDelta(delta map[string]any) error {
	field := ""
	switch stringValue(delta["type"]) {
	case "text_delta":
		field = "text"
	case "input_json_delta":
		field = "partial_json"
	case "thinking_delta":
		field = "thinking"
	case "signature_delta":
		field = "signature"
	default:
		return fmt.Errorf("unsupported Messages stream delta type %q", stringValue(delta["type"]))
	}
	if _, ok := delta[field].(string); !ok {
		return fmt.Errorf("%s %s must be a string", stringValue(delta["type"]), field)
	}
	return nil
}

func stripMessagesHostedSearchBlocks(root map[string]any) bool {
	content, ok := root["content"].([]any)
	if !ok {
		return false
	}
	filtered := make([]any, 0, len(content))
	changed := false
	for _, raw := range content {
		block, _ := raw.(map[string]any)
		switch stringValue(block["type"]) {
		case "server_tool_use", "web_search_tool_result":
			changed = true
		default:
			filtered = append(filtered, raw)
		}
	}
	if changed {
		root["content"] = filtered
	}
	return changed
}

func validateChatEnvelope(root map[string]any) error {
	if strings.TrimSpace(stringValue(root["id"])) == "" {
		return fmt.Errorf("Chat Completions response id must be a non-empty string")
	}
	object := stringValue(root["object"])
	if object != "chat.completion" && object != "chat.completion.chunk" {
		return fmt.Errorf("Chat Completions response object must be %q", "chat.completion")
	}
	choices, ok := root["choices"].([]any)
	if !ok || len(choices) == 0 {
		return fmt.Errorf("Chat Completions response choices must be a non-empty array")
	}
	for index, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok || choice == nil {
			return fmt.Errorf("Chat Completions response choices[%d] must be an object", index)
		}
		message, ok := choice["message"].(map[string]any)
		if !ok || message == nil {
			return fmt.Errorf("Chat Completions response choices[%d].message must be an object", index)
		}
	}
	return nil
}

func validateResponsesEventPayload(data []byte) error {
	event, err := decodeJSONMap(data)
	if err != nil {
		return err
	}
	value, exists := event["response"]
	if !exists {
		return nil
	}
	response, ok := value.(map[string]any)
	if !ok || response == nil {
		return fmt.Errorf("Responses event response must be an object")
	}
	return validateResponsesEnvelope(response)
}

// Chat Completions has no standard backend-tool-call item. Do not claim that a
// search ran merely because the request offered a search extension: require an
// upstream citation/result or an explicit server-side search usage counter.
func chatSearchExecuted(root, choice, message map[string]any, urls []string) bool {
	if len(urls) > 0 {
		return true
	}
	for _, values := range []map[string]any{root, choice, message} {
		for _, key := range []string{"search_results", "web_search_results", "sources", "citations", "annotations"} {
			if nonEmptyJSONValue(values[key]) {
				return true
			}
		}
	}
	usage, _ := root["usage"].(map[string]any)
	return positiveSearchUsage(usage)
}

func positiveSearchUsage(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			switch strings.ToLower(key) {
			case "num_sources_used", "web_search_requests", "search_requests", "search_queries_count":
				if numberInt(child) > 0 {
					return true
				}
			}
			if positiveSearchUsage(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if positiveSearchUsage(child) {
				return true
			}
		}
	}
	return false
}

func nonEmptyJSONValue(value any) bool {
	switch typed := value.(type) {
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	case string:
		return strings.TrimSpace(typed) != ""
	default:
		return false
	}
}

func canonicalResponse(route config.Route, request facadeRequest, result canonicalResult) map[string]any {
	now := time.Now().Unix()
	status := "completed"
	var incomplete any
	if result.IncompleteReason != "" {
		status = "incomplete"
		incomplete = map[string]any{"reason": result.IncompleteReason}
	}
	total := result.InputTokens + result.OutputTokens
	output := result.Output
	if output == nil {
		output = []any{}
	}
	return map[string]any{
		"id":                     compatID("resp"),
		"object":                 "response",
		"created_at":             now,
		"completed_at":           now,
		"status":                 status,
		"background":             false,
		"error":                  nil,
		"incomplete_details":     incomplete,
		"instructions":           nil,
		"max_output_tokens":      nil,
		"max_tool_calls":         nil,
		"metadata":               map[string]any{},
		"model":                  route.WireModel,
		"output":                 output,
		"parallel_tool_calls":    true,
		"previous_response_id":   nil,
		"prompt_cache_key":       nil,
		"prompt_cache_retention": nil,
		"reasoning":              map[string]any{"effort": nil, "summary": nil},
		"safety_identifier":      nil,
		"service_tier":           "default",
		"store":                  false,
		"temperature":            1,
		"text":                   map[string]any{"format": map[string]any{"type": "text"}, "verbosity": nil},
		"tool_choice":            "auto",
		"tools":                  responseTools(request.HostedWebSearch),
		"top_logprobs":           0,
		"top_p":                  1,
		"truncation":             "disabled",
		"usage": map[string]any{
			"input_tokens":  result.InputTokens,
			"output_tokens": result.OutputTokens,
			"total_tokens":  total,
			"input_tokens_details": map[string]any{
				"cached_tokens": result.CachedTokens,
			},
			"output_tokens_details": map[string]any{
				"reasoning_tokens": result.ReasoningTokens,
			},
		},
		"user": nil,
	}
}

func responseTools(hosted bool) []any {
	if !hosted {
		return []any{}
	}
	return []any{map[string]any{"type": "web_search", "search_context_size": nil, "user_location": nil}}
}

func writeCanonicalResponse(w http.ResponseWriter, response map[string]any, stream bool) error {
	if response == nil {
		return fmt.Errorf("response body must be a JSON object")
	}
	if err := validateResponsesEnvelope(response); err != nil {
		return err
	}
	if _, err := json.Marshal(response); err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	if !stream {
		data, err := json.Marshal(response)
		if err != nil {
			return fmt.Errorf("encode response: %w", err)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(data)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return nil
	}
	output := response["output"].([]any)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	sequence := 0
	emit := func(typ string, values map[string]any) {
		values["type"] = typ
		values["sequence_number"] = sequence
		sequence++
		data, _ := json.Marshal(values)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, data)
		if flusher != nil {
			flusher.Flush()
		}
	}
	created := cloneMap(response)
	created["completed_at"] = nil
	created["status"] = "in_progress"
	created["output"] = []any{}
	created["usage"] = nil
	emit("response.created", map[string]any{"response": created})
	emit("response.in_progress", map[string]any{"response": cloneMap(created)})

	for index, raw := range output {
		item := raw.(map[string]any)
		emitOutputItem(emit, index, item)
	}
	terminal := "response.completed"
	if stringValue(response["status"]) == "incomplete" {
		terminal = "response.incomplete"
	}
	emit(terminal, map[string]any{"response": response})
	return nil
}

func emitOutputItem(emit func(string, map[string]any), index int, item map[string]any) {
	typ := stringValue(item["type"])
	id := stringValue(item["id"])
	added := cloneMap(item)
	added["status"] = "in_progress"
	switch typ {
	case "reasoning":
		added["content"] = []any{}
		emit("response.output_item.added", map[string]any{"output_index": index, "item": added})
		for contentIndex, rawPart := range anySlice(item["content"]) {
			part, _ := rawPart.(map[string]any)
			text := stringValue(part["text"])
			emit("response.content_part.added", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "part": map[string]any{"type": "reasoning_text", "text": ""}})
			if text != "" {
				emit("response.reasoning_text.delta", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "delta": text})
			}
			emit("response.reasoning_text.done", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "text": text})
			emit("response.content_part.done", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "part": part})
		}
		emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
	case "web_search_call":
		emit("response.output_item.added", map[string]any{"output_index": index, "item": added})
		emit("response.web_search_call.in_progress", map[string]any{"output_index": index, "item_id": id})
		emit("response.web_search_call.searching", map[string]any{"output_index": index, "item_id": id})
		emit("response.web_search_call.completed", map[string]any{"output_index": index, "item_id": id})
		emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
	case "function_call":
		arguments := stringValue(item["arguments"])
		added["arguments"] = ""
		emit("response.output_item.added", map[string]any{"output_index": index, "item": added})
		if arguments != "" {
			emit("response.function_call_arguments.delta", map[string]any{"output_index": index, "item_id": id, "delta": arguments})
		}
		emit("response.function_call_arguments.done", map[string]any{"output_index": index, "item_id": id, "arguments": arguments})
		emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
	case "message":
		added["content"] = []any{}
		emit("response.output_item.added", map[string]any{"output_index": index, "item": added})
		for contentIndex, rawPart := range anySlice(item["content"]) {
			part, _ := rawPart.(map[string]any)
			emptyPart := cloneMap(part)
			switch stringValue(part["type"]) {
			case "refusal":
				refusal := stringValue(part["refusal"])
				emptyPart["refusal"] = ""
				emit("response.content_part.added", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "part": emptyPart})
				if refusal != "" {
					emit("response.refusal.delta", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "delta": refusal})
				}
				emit("response.refusal.done", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "refusal": refusal})
			default:
				text := stringValue(part["text"])
				emptyPart["text"] = ""
				emit("response.content_part.added", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "part": emptyPart})
				if text != "" {
					emit("response.output_text.delta", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "delta": text, "logprobs": []any{}})
				}
				emit("response.output_text.done", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "text": text, "logprobs": []any{}})
			}
			emit("response.content_part.done", map[string]any{"output_index": index, "item_id": id, "content_index": contentIndex, "part": part})
		}
		emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
	default:
		emit("response.output_item.added", map[string]any{"output_index": index, "item": added})
		emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
	}
}

func reasoningItem(text, encrypted string) map[string]any {
	item := map[string]any{"type": "reasoning", "id": compatID("rs"), "status": "completed", "summary": []any{}, "content": []any{}}
	if text != "" {
		item["content"] = []any{map[string]any{"type": "reasoning_text", "text": text}}
	}
	if encrypted != "" {
		item["encrypted_content"] = encrypted
	}
	return item
}

func messageItem(text string, annotations []any) map[string]any {
	if annotations == nil {
		annotations = []any{}
	}
	return map[string]any{
		"type": "message", "id": compatID("msg"), "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": annotations, "logprobs": []any{}}},
	}
}

func functionCallItem(id, name, arguments string) map[string]any {
	if id == "" {
		id = compatID("call")
	}
	return map[string]any{"type": "function_call", "id": compatID("fc"), "call_id": id, "name": name, "arguments": arguments, "status": "completed"}
}

func webSearchItem(id, query string, sources []any, status string) map[string]any {
	if id == "" {
		id = compatID("ws")
	}
	if sources == nil {
		sources = []any{}
	}
	return map[string]any{"type": "web_search_call", "id": id, "status": status, "action": map[string]any{"type": "search", "query": query, "sources": sources}}
}

func messageSearchSources(value any) ([]any, bool) {
	var sources []any
	failed := false
	if errorBlock, _ := value.(map[string]any); errorBlock != nil {
		failed = strings.Contains(stringValue(errorBlock["type"]), "error")
	}
	for _, raw := range anySlice(value) {
		entry, _ := raw.(map[string]any)
		if stringValue(entry["type"]) != "web_search_result" {
			continue
		}
		if rawURL := stringValue(entry["url"]); rawURL != "" {
			source := map[string]any{"type": "url", "url": rawURL}
			if title := stringValue(entry["title"]); title != "" {
				source["title"] = title
			}
			sources = append(sources, source)
		}
	}
	return sources, failed
}

func chatMessageText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	var parts []string
	for _, raw := range anySlice(value) {
		part, _ := raw.(map[string]any)
		if typ := stringValue(part["type"]); typ == "text" || typ == "output_text" {
			parts = append(parts, stringValue(part["text"]))
		}
	}
	return strings.Join(parts, "")
}

func collectCitationURLs(values ...map[string]any) []string {
	seen := map[string]bool{}
	var urls []string
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				if (key == "url" || key == "uri") && stringValue(child) != "" {
					u := stringValue(child)
					if strings.HasPrefix(u, "http") && !seen[u] {
						seen[u] = true
						urls = append(urls, u)
					}
				}
				if key == "citations" || key == "annotations" || key == "search_results" || key == "sources" {
					walk(child)
				}
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		case string:
			if strings.HasPrefix(v, "http") && !seen[v] {
				seen[v] = true
				urls = append(urls, v)
			}
		}
	}
	for _, value := range values {
		walk(value)
	}
	sort.Strings(urls)
	return urls
}

func citationsToAnnotations(value any) []any {
	var root map[string]any
	if value != nil {
		root = map[string]any{"citations": value}
	}
	return urlsToAnnotations(collectCitationURLs(root))
}

func urlsToAnnotations(urls []string) []any {
	annotations := make([]any, 0, len(urls))
	for _, rawURL := range urls {
		annotations = append(annotations, map[string]any{"type": "url_citation", "url": rawURL, "title": rawURL, "start_index": 0, "end_index": 0})
	}
	return annotations
}

func urlsToSources(urls []string) []any {
	sources := make([]any, 0, len(urls))
	for _, rawURL := range urls {
		sources = append(sources, map[string]any{"type": "url", "url": rawURL})
	}
	return sources
}

// backfillResponseSearchSources supplies the URL list consumed by Grok Build's
// native "(N sites)" renderer when an upstream confirms that search ran but
// omits structured citations. Free-form links alone never create a search call.
func backfillResponseSearchSources(response map[string]any, hosted bool, query string) bool {
	output, ok := response["output"].([]any)
	if !ok {
		return false
	}

	var calls []map[string]any
	var structuredURLs []string
	var textURLs []string
	firstMessage := len(output)
	for index, raw := range output {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		switch stringValue(item["type"]) {
		case "web_search_call":
			calls = append(calls, item)
			if action, _ := item["action"].(map[string]any); action != nil {
				structuredURLs = mergeUniqueStrings(structuredURLs, urlsFromJSON(action["sources"])...)
			}
		case "message":
			if firstMessage == len(output) {
				firstMessage = index
			}
			for _, rawPart := range anySlice(item["content"]) {
				part, _ := rawPart.(map[string]any)
				if part == nil || stringValue(part["type"]) != "output_text" {
					continue
				}
				structuredURLs = mergeUniqueStrings(structuredURLs, urlsFromJSON(part["annotations"])...)
				textURLs = mergeUniqueStrings(textURLs, urlsFromText(stringValue(part["text"]))...)
			}
		}
	}

	confirmed := len(calls) > 0 || (hosted && (len(structuredURLs) > 0 || positiveSearchUsage(response["usage"])))
	if !confirmed {
		return false
	}
	changed := false
	if len(calls) == 0 {
		call := webSearchItem("", query, nil, "completed")
		output = append(output, nil)
		copy(output[firstMessage+1:], output[firstMessage:])
		output[firstMessage] = call
		response["output"] = output
		calls = append(calls, call)
		changed = true
	}

	allURLs := mergeUniqueStrings(structuredURLs, textURLs...)
	if len(allURLs) > maxBackfilledSearchSources {
		allURLs = allURLs[:maxBackfilledSearchSources]
	}
	target := calls[len(calls)-1]
	action, _ := target["action"].(map[string]any)
	if action == nil {
		action = map[string]any{"type": "search", "query": query, "sources": []any{}}
		target["action"] = action
	}
	if strings.TrimSpace(stringValue(action["query"])) == "" {
		action["query"] = query
	}
	if mergeResponseSearchURLs(response, allURLs) {
		changed = true
	}
	// Grok Build has two search render paths. Native backend search reads
	// action.sources, while its client-side web_search tool extracts only URL
	// citations from output_text annotations. Mirror the same verified URLs to
	// both representations so non-hosted model channels get the native site
	// count too.
	if mergeResponseCitationURLs(response, allURLs) {
		changed = true
	}
	return changed
}

func mergeResponseSearchURLs(response map[string]any, urls []string) bool {
	var target map[string]any
	for _, raw := range anySlice(response["output"]) {
		item, _ := raw.(map[string]any)
		if item != nil && stringValue(item["type"]) == "web_search_call" {
			target = item
		}
	}
	if target == nil || len(urls) == 0 {
		return false
	}
	action, _ := target["action"].(map[string]any)
	before := len(anySlice(action["sources"]))
	mergeWebSearchSources(target, urlsToSources(urls))
	return len(anySlice(action["sources"])) > before
}

func mergeResponseCitationURLs(response map[string]any, urls []string) bool {
	if len(urls) == 0 {
		return false
	}
	seen := map[string]bool{}
	var target map[string]any
	for _, raw := range anySlice(response["output"]) {
		item, _ := raw.(map[string]any)
		if stringValue(item["type"]) != "message" {
			continue
		}
		for _, rawPart := range anySlice(item["content"]) {
			part, _ := rawPart.(map[string]any)
			if stringValue(part["type"]) != "output_text" {
				continue
			}
			if target == nil {
				target = part
			}
			for _, rawURL := range urlsFromJSON(part["annotations"]) {
				seen[rawURL] = true
			}
		}
	}
	if target == nil {
		return false
	}
	missing := make([]string, 0, len(urls))
	for _, rawURL := range urls {
		if rawURL != "" && !seen[rawURL] {
			seen[rawURL] = true
			missing = append(missing, rawURL)
		}
	}
	if len(missing) == 0 {
		return false
	}
	target["annotations"] = mergeAnnotations(anySlice(target["annotations"]), urlsToAnnotations(missing))
	return true
}

func urlsFromText(text string) []string {
	seen := map[string]bool{}
	urls := make([]string, 0)
	for _, match := range httpURLInTextPattern.FindAllString(text, -1) {
		candidate := strings.TrimRight(match, ".,;:!?`*_~")
		parsed, err := url.Parse(candidate)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || seen[candidate] {
			continue
		}
		seen[candidate] = true
		urls = append(urls, candidate)
		if len(urls) == maxBackfilledSearchSources {
			break
		}
	}
	return urls
}

func decodeJSONMap(data []byte) (map[string]any, error) {
	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("response body must be a JSON object")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	return root, nil
}

func numberInt(value any) int {
	return positiveInt(value, 0)
}

func valueFirst(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

func cloneMap(value map[string]any) map[string]any {
	data, _ := json.Marshal(value)
	var clone map[string]any
	_ = json.Unmarshal(data, &clone)
	return clone
}

func compatID(prefix string) string {
	var data [12]byte
	_, _ = rand.Read(data[:])
	return prefix + "_" + hex.EncodeToString(data[:])
}
