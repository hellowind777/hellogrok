package patch

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	reEmptyFinish = regexp.MustCompile(`("(?:native_)?finish_reason"\s*:\s*)""`)
)

// Options controls how we rewrite upstream JSON for Grok Build.
type Options struct {
	GPTResponses            bool
	WebSearch               bool
	RequestModel            string
	ContextDetailsFromUsage bool
}

// Output-item types that Grok's Responses deserializer typically requires an id on.
var needIDTypes = map[string]string{
	"message":               "msg",
	"reasoning":             "rs",
	"function_call":         "fc",
	"function_call_output":  "fco",
	"custom_tool_call":      "ctc",
	"web_search_call":       "wsc",
	"file_search_call":      "fsc",
	"code_interpreter_call": "cic",
	"image_generation_call": "igc",
	"mcp_call":              "mcp",
	"mcp_list_tools":        "mlt",
	"mcp_approval_request":  "mar",
	"compactions":           "cmp",
	"compaction":            "cmp",
	"item_reference":        "ref",
}

// Only output_text carries annotations in Build's async-openai response model.
// InputTextContent contains text only.
var contentPartTypes = map[string]bool{
	"output_text": true,
	// deliberately exclude bare "text" - that is ResponseTextConfig.format.type
}

// JSONObject patches a decoded JSON value in place.
func JSONObject(v any, opt Options) {
	walkPatch(v, opt, "")
}

func walkPatch(v any, opt Options, ctx string) {
	switch t := v.(type) {
	case map[string]any:
		patchMap(t, opt, ctx)
		parentIsResp := isResponseObject(t)
		parentType, _ := t["type"].(string)
		for k, child := range t {
			next := ctx
			switch {
			case k == "output":
				next = "output"
				if strings.HasPrefix(ctx, "response:") {
					next += ":" + strings.TrimPrefix(ctx, "response:")
				}
			case k == "content":
				next = "content"
			case k == "item":
				next = "item:" + parentType
			case k == "part":
				next = "part"
			case k == "response":
				next = "response:" + parentType
			case k == "format" || k == "text":
				// response.text / text.format are config, NOT content parts
				if parentIsResp || ctx == "response" || ctx == "config" {
					next = "config"
				}
			case ctx == "config":
				next = "config"
			}
			if k == "usage" {
				continue
			}
			walkPatch(child, opt, next)
		}
	case []any:
		for _, child := range t {
			walkPatch(child, opt, ctx)
		}
	}
}

func patchMap(m map[string]any, opt Options, ctx string) {
	// Chat Completions: empty finish_reason string → null
	if fr, ok := m["finish_reason"].(string); ok && fr == "" {
		m["finish_reason"] = nil
	}
	if fr, ok := m["native_finish_reason"].(string); ok && fr == "" {
		m["native_finish_reason"] = nil
	}

	// Do not treat ResponseTextConfig / format objects as content parts.
	if ctx == "config" {
		return
	}

	// Every Responses SSE event carries a sequence_number. Some gateways omit
	// it only from response.created while numbering all following events from 1.
	if typ, _ := m["type"].(string); strings.HasPrefix(typ, "response.") {
		if missingOrEmpty(m, "sequence_number") {
			m["sequence_number"] = 0
		}
	}

	// Response object (top-level or nested under "response")
	if isResponseObject(m) {
		ensureResponse(m, opt, ctx)
	}

	// SSE envelope fields
	if item, ok := m["item"].(map[string]any); ok {
		ensureOutputItem(item, stringValue(m["type"]))
		ensureContentParts(item)
	}
	if part, ok := m["part"].(map[string]any); ok {
		ensureContentPart(part)
	}

	// Direct content part objects (only real content types)
	if typ, _ := m["type"].(string); contentPartTypes[typ] {
		ensureContentPart(m)
	}

	// Output items by type, or objects sitting in output/item context
	if typ, ok := m["type"].(string); ok {
		if _, need := needIDTypes[typ]; need {
			ensureOutputItem(m, ctx)
			if typ == "message" || typ == "reasoning" {
				ensureContentParts(m)
			}
		}
	} else if strings.HasPrefix(ctx, "output") || strings.HasPrefix(ctx, "item") {
		// gateway omitted type but has role/content — treat as message
		if _, hasRole := m["role"]; hasRole {
			if _, ok := m["type"]; !ok {
				m["type"] = "message"
			}
			ensureOutputItem(m, ctx)
			ensureContentParts(m)
		}
	}

	// Assistant/user message without type (some gateways)
	if role, _ := m["role"].(string); (role == "assistant" || role == "user") && ctx != "config" {
		if missingOrEmpty(m, "id") {
			m["id"] = newID("msg")
		}
		if _, ok := m["type"]; !ok {
			// only if it looks like an output item (has content or status)
			if _, has := m["content"]; has {
				m["type"] = "message"
				ensureOutputItem(m, ctx)
			}
		}
		ensureContentParts(m)
	}
}

func isResponseObject(m map[string]any) bool {
	if obj, _ := m["object"].(string); obj == "response" {
		return true
	}
	// Nested response snapshots often omit object but have status+output
	_, hasOut := m["output"]
	_, hasStatus := m["status"]
	_, hasID := m["id"]
	if hasOut && (hasStatus || hasID) {
		// avoid matching random maps: require created_at or model or usage or object-ish keys
		if _, ok := m["created_at"]; ok {
			return true
		}
		if _, ok := m["model"]; ok {
			return true
		}
		if _, ok := m["usage"]; ok {
			return true
		}
	}
	return false
}

func ensureResponse(m map[string]any, opt Options, ctx string) {
	if missingOrEmpty(m, "id") {
		m["id"] = newID("resp")
	}
	if missingOrEmpty(m, "created_at") {
		m["created_at"] = time.Now().Unix()
	}
	if missingOrEmpty(m, "object") {
		m["object"] = "response"
	}
	if missingOrEmpty(m, "status") {
		m["status"] = responseStatusForContext(ctx)
	}
	if opt.RequestModel != "" && missingOrEmpty(m, "model") {
		m["model"] = opt.RequestModel
	}
	// DeepSeek accepts a hosted search selector in requests and echoes it in
	// the response. Build's pinned async-openai response model does not include
	// web_search in ToolChoiceTypes, so preserve the executed tools/output while
	// normalizing only this descriptive response field.
	if choice, _ := m["tool_choice"].(map[string]any); choice != nil {
		typ := strings.ToLower(strings.TrimSpace(stringValue(choice["type"])))
		if typ == "x_search" || typ == "web_search" || strings.HasPrefix(typ, "web_search_") {
			m["tool_choice"] = "auto"
		}
	}
	if output, ok := m["output"]; !ok || output == nil {
		m["output"] = []any{}
	}
	if arr, ok := m["output"].([]any); ok {
		for _, el := range arr {
			if om, ok := el.(map[string]any); ok {
				ensureOutputItem(om, ctx)
				ensureContentParts(om)
			}
		}
	}
	usage, ok := m["usage"].(map[string]any)
	contextInput, contextOutput, addContextDetails := contextDetailsFromUsage(usage, opt.ContextDetailsFromUsage)
	if !ok || !normalizeUsageTokenFields(usage) {
		// Grok Build treats total_tokens as the current complete context size.
		// An invented or partial value would corrupt that counter and postpone
		// auto-compaction, so an unusable provider measurement stays unknown.
		m["usage"] = nil
		return
	}
	if addContextDetails {
		usage["context_details"] = map[string]any{
			"input_tokens":  contextInput,
			"output_tokens": contextOutput,
		}
	}
}

func responseStatusForContext(ctx string) string {
	event := strings.TrimPrefix(ctx, "response:")
	switch event {
	case "response.created", "response.in_progress":
		return "in_progress"
	case "response.queued":
		return "queued"
	case "response.failed":
		return "failed"
	case "response.incomplete":
		return "incomplete"
	default:
		return "completed"
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func ensureContentPart(m map[string]any) {
	typ, _ := m["type"].(string)
	if typ == "" {
		// infer output_text if it has text field in content context
		if _, has := m["text"]; has {
			m["type"] = "output_text"
			typ = "output_text"
		}
	}
	if !contentPartTypes[typ] {
		return
	}
	if _, ok := m["annotations"]; !ok {
		m["annotations"] = []any{}
	}
	// null annotations → empty array (Grok expects Vec, not null)
	if m["annotations"] == nil {
		m["annotations"] = []any{}
	}
	if typ == "output_text" {
		if _, ok := m["logprobs"]; !ok {
			m["logprobs"] = []any{}
		}
		if m["logprobs"] == nil {
			m["logprobs"] = []any{}
		}
	}
	if _, ok := m["text"]; !ok {
		m["text"] = ""
	}
}

func ensureContentParts(m map[string]any) {
	arr, ok := m["content"].([]any)
	if !ok {
		return
	}
	for _, el := range arr {
		part, ok := el.(map[string]any)
		if !ok {
			continue
		}
		ensureContentPart(part)
	}
}

func ensureOutputItem(m map[string]any, contexts ...string) {
	ctx := ""
	if len(contexts) > 0 {
		ctx = contexts[0]
	}
	defaultStatus := "completed"
	if strings.Contains(ctx, "response.output_item.added") ||
		strings.Contains(ctx, "response.created") || strings.Contains(ctx, "response.in_progress") {
		defaultStatus = "in_progress"
	}
	typ, _ := m["type"].(string)
	prefix := "item"
	if p, ok := needIDTypes[typ]; ok {
		prefix = p
	} else if typ == "" {
		if _, has := m["role"]; has {
			typ = "message"
			m["type"] = "message"
			prefix = "msg"
		}
	}
	if missingOrEmpty(m, "id") {
		m["id"] = newID(prefix)
	}
	switch typ {
	case "message":
		if missingOrEmpty(m, "status") {
			m["status"] = defaultStatus
		}
		if missingOrEmpty(m, "role") {
			m["role"] = "assistant"
		}
		if content, ok := m["content"]; !ok || content == nil {
			m["content"] = []any{}
		}
	case "reasoning":
		if missingOrEmpty(m, "status") {
			m["status"] = defaultStatus
		}
		// Build deserializes this as Vec<SummaryPart>; null is invalid.
		if summary, ok := m["summary"]; !ok || summary == nil {
			m["summary"] = []any{}
		}
	case "function_call":
		if missingOrEmpty(m, "status") {
			m["status"] = defaultStatus
		}
		if arguments, ok := m["arguments"]; !ok || arguments == nil {
			m["arguments"] = "{}"
		}
		if missingOrEmpty(m, "call_id") {
			if id, ok := m["id"].(string); ok && id != "" {
				m["call_id"] = id
			} else {
				m["call_id"] = newID("call")
			}
		}
	case "custom_tool_call":
		// CustomToolCall is a distinct Build type. It requires input/name and
		// does not define FunctionToolCall's status/arguments fields.
		if input, ok := m["input"]; !ok || input == nil {
			if arguments, exists := m["arguments"]; exists {
				m["input"] = stringifyToolInput(arguments)
			} else {
				m["input"] = ""
			}
		}
		if missingOrEmpty(m, "name") {
			m["name"] = "x_search"
		}
		if missingOrEmpty(m, "call_id") {
			if id, ok := m["id"].(string); ok && id != "" {
				m["call_id"] = id
			} else {
				m["call_id"] = newID("call")
			}
		}
	case "function_call_output":
		if missingOrEmpty(m, "call_id") {
			m["call_id"] = newID("call")
		}
	case "web_search_call":
		if missingOrEmpty(m, "status") {
			m["status"] = defaultStatus
		}
		ensureWebSearchAction(m)
	case "file_search_call", "code_interpreter_call", "image_generation_call", "mcp_call":
		if _, ok := m["status"]; !ok {
			m["status"] = defaultStatus
		}
	}
}

func stringifyToolInput(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func ensureWebSearchAction(item map[string]any) {
	action, _ := item["action"].(map[string]any)
	if action == nil {
		action = map[string]any{}
		item["action"] = action
	}
	typ, _ := action["type"].(string)
	if typ == "" {
		typ = "search"
		action["type"] = typ
	}
	if typ != "search" {
		return
	}
	if missingOrEmpty(action, "query") {
		action["query"] = firstProviderSearchQuery(action["queries"])
	}
	if _, ok := action["sources"]; !ok {
		action["sources"] = []any{}
	}
}

func firstProviderSearchQuery(value any) string {
	queries, _ := value.([]any)
	for _, value := range queries {
		query, _ := value.(string)
		query = strings.TrimSpace(query)
		if query != "" && !strings.HasPrefix(query, "ws_call_id=") {
			return query
		}
	}
	return ""
}

func missingOrEmpty(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok || v == nil {
		return true
	}
	if s, ok := v.(string); ok && s == "" {
		return true
	}
	return false
}

const maxWireTokenCount = int64(^uint32(0))

func contextDetailsFromUsage(usage map[string]any, enabled bool) (int64, int64, bool) {
	if !enabled || usage == nil {
		return 0, 0, false
	}
	// Never replace a first-party measurement, including a partial extension
	// that Grok Build will deliberately ignore instead of guessing.
	if _, exists := usage["context_details"]; exists {
		return 0, 0, false
	}
	input, hasInput, validInput := canonicalUsageTokenCount(usage, "input_tokens", "prompt_tokens")
	output, hasOutput, validOutput := canonicalUsageTokenCount(usage, "output_tokens", "completion_tokens")
	if !hasInput || !hasOutput || !validInput || !validOutput || input > maxWireTokenCount-output {
		return 0, 0, false
	}
	if input == 0 && output == 0 {
		return 0, 0, false
	}
	return input, output, true
}

// normalizeUsageTokenFields accepts only complete billing measurements. Grok
// Build requires input/output fields for its usage ledger and uses total for
// context tracking, so accepting total alone would require inventing ledger
// values. A provider total is preserved when the complete pair is present;
// otherwise the total is derived from that pair.
func normalizeUsageTokenFields(usage map[string]any) bool {
	input, hasInput, validInput := canonicalUsageTokenCount(usage, "input_tokens", "prompt_tokens")
	output, hasOutput, validOutput := canonicalUsageTokenCount(usage, "output_tokens", "completion_tokens")
	total, hasTotal, validTotal := optionalTokenCount(usage, "total_tokens")
	if !validInput || !validOutput || !validTotal || !hasInput || !hasOutput {
		return false
	}
	if !hasTotal {
		if input > maxWireTokenCount-output {
			return false
		}
		total = input + output
		usage["total_tokens"] = total
	}
	if input == 0 && output == 0 && total == 0 && !positiveContextDetails(usage) {
		return false
	}
	copyUsageDetailAlias(usage, "input_tokens_details", "prompt_tokens_details")
	copyUsageDetailAlias(usage, "output_tokens_details", "completion_tokens_details")
	if !normalizeUsageDetail(usage, "output_tokens_details", "reasoning_tokens") ||
		!normalizeUsageDetail(usage, "input_tokens_details", "cached_tokens") {
		return false
	}
	return total >= 0
}

func canonicalUsageTokenCount(usage map[string]any, canonical string, aliases ...string) (int64, bool, bool) {
	value, present, valid := optionalTokenCount(usage, canonical)
	if present || !valid {
		return value, present, valid
	}
	for _, alias := range aliases {
		value, present, valid = optionalTokenCount(usage, alias)
		if !valid || present {
			if present && valid {
				usage[canonical] = value
			}
			return value, present, valid
		}
	}
	return 0, false, true
}

func copyUsageDetailAlias(usage map[string]any, canonical, alias string) {
	if value, exists := usage[canonical]; exists && value != nil {
		return
	}
	if value, exists := usage[alias]; exists && value != nil {
		usage[canonical] = value
	}
}

func positiveContextDetails(usage map[string]any) bool {
	details, _ := usage["context_details"].(map[string]any)
	input, hasInput, validInput := optionalTokenCount(details, "input_tokens")
	output, hasOutput, validOutput := optionalTokenCount(details, "output_tokens")
	return hasInput && hasOutput && validInput && validOutput &&
		input <= maxWireTokenCount-output && input+output > 0
}

func normalizeUsageDetail(usage map[string]any, detailKey, tokenKey string) bool {
	raw, exists := usage[detailKey]
	if !exists || raw == nil {
		usage[detailKey] = map[string]any{tokenKey: int64(0)}
		return true
	}
	details, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	_, present, valid := optionalTokenCount(details, tokenKey)
	if !valid {
		return false
	}
	if !present {
		details[tokenKey] = int64(0)
	}
	return true
}

func optionalTokenCount(values map[string]any, key string) (int64, bool, bool) {
	value, exists := values[key]
	if !exists || value == nil {
		return 0, false, true
	}
	var count int64
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Int64()
		if err != nil {
			return 0, true, false
		}
		count = parsed
	case float64:
		if number < 0 || number > float64(maxWireTokenCount) || number != float64(int64(number)) {
			return 0, true, false
		}
		count = int64(number)
	case int:
		count = int64(number)
	case int64:
		count = number
	case uint64:
		if number > uint64(maxWireTokenCount) {
			return 0, true, false
		}
		count = int64(number)
	default:
		return 0, true, false
	}
	return count, true, count >= 0 && count <= maxWireTokenCount
}

// PatchJSONBytes patches a full JSON document.
func PatchJSONBytes(data []byte, opt Options) []byte {
	out, err := PatchJSONBytesStrict(data, opt)
	if err != nil {
		return []byte(reEmptyFinish.ReplaceAllString(string(data), `${1}null`))
	}
	return out
}

// PatchJSONBytesStrict patches one complete JSON document and rejects trailing
// content. The proxy uses this path so a truncated or concatenated 2xx body is
// reported as an upstream error instead of being forwarded as valid Responses.
func PatchJSONBytesStrict(data []byte, opt Options) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var v any
	if err := decoder.Decode(&v); err != nil {
		return nil, err
	}
	if v == nil {
		return nil, fmt.Errorf("response body must be a JSON object")
	}
	if _, ok := v.(map[string]any); !ok {
		return nil, fmt.Errorf("response body must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("multiple JSON values")
		}
		return nil, err
	}
	JSONObject(v, opt)
	out, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PatchSSEDataLine patches one SSE "data: {...}" line.
func PatchSSEDataLine(line string, opt Options) string {
	return PatchSSEDataLineWithSequence(line, opt, 0)
}

// PatchSSEDataLineWithSequence uses fallbackSequence only when a Responses
// event omitted its required sequence_number. Existing upstream values win.
func PatchSSEDataLineWithSequence(line string, opt Options, fallbackSequence int) string {
	if !strings.HasPrefix(line, "data:") {
		return line
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return line
	}
	var v any
	if err := json.Unmarshal([]byte(payload), &v); err != nil {
		fixed := reEmptyFinish.ReplaceAllString(payload, `${1}null`)
		return "data: " + fixed
	}
	if event, ok := v.(map[string]any); ok {
		if typ, _ := event["type"].(string); strings.HasPrefix(typ, "response.") &&
			missingOrEmpty(event, "sequence_number") {
			event["sequence_number"] = fallbackSequence
		}
	}
	JSONObject(v, opt)
	out, err := json.Marshal(v)
	if err != nil {
		return line
	}
	return "data: " + string(out)
}

// FindMissing reports Grok-critical fields missing from a decoded JSON value.
// Used for first-request diagnostics (before patching).
func FindMissing(v any) []string {
	var out []string
	findMissingWalk(v, "$", &out)
	// de-dupe + sort for stable logs
	seen := map[string]struct{}{}
	var uniq []string
	for _, s := range out {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		uniq = append(uniq, s)
	}
	sort.Strings(uniq)
	return uniq
}

// FindMissingJSON unmarshals then FindMissing.
func FindMissingJSON(data []byte) []string {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil
	}
	return FindMissing(v)
}

// FindMissingSSELine analyzes one data: line.
func FindMissingSSELine(line string) []string {
	if !strings.HasPrefix(line, "data:") {
		return nil
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return nil
	}
	return FindMissingJSON([]byte(payload))
}

func findMissingWalk(v any, path string, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		// skip config: response.text.format
		if isFormatConfig(t) {
			return
		}
		if isResponseObject(t) {
			if missingOrEmpty(t, "id") {
				*out = append(*out, path+".id")
			}
			if missingOrEmpty(t, "created_at") {
				*out = append(*out, path+".created_at")
			}
			if output, ok := t["output"]; !ok || output == nil {
				*out = append(*out, path+".output")
			}
			if missingOrEmpty(t, "object") {
				*out = append(*out, path+".object")
			}
			if missingOrEmpty(t, "status") {
				*out = append(*out, path+".status")
			}
			if missingOrEmpty(t, "model") {
				*out = append(*out, path+".model")
			}
			if usage, ok := t["usage"].(map[string]any); ok {
				for _, field := range []string{"input_tokens", "output_tokens", "total_tokens"} {
					if value, exists := usage[field]; !exists || value == nil {
						*out = append(*out, path+".usage."+field)
					}
				}
				if otd, _ := usage["output_tokens_details"].(map[string]any); otd == nil {
					*out = append(*out, path+".usage.output_tokens_details")
				} else if _, ok := otd["reasoning_tokens"]; !ok {
					*out = append(*out, path+".usage.output_tokens_details.reasoning_tokens")
				}
				if itd, _ := usage["input_tokens_details"].(map[string]any); itd == nil {
					*out = append(*out, path+".usage.input_tokens_details")
				} else if _, ok := itd["cached_tokens"]; !ok {
					*out = append(*out, path+".usage.input_tokens_details.cached_tokens")
				}
			}
		}
		if typ, _ := t["type"].(string); typ != "" {
			if strings.HasPrefix(typ, "response.") && missingOrEmpty(t, "sequence_number") {
				*out = append(*out, path+".sequence_number")
			}
			if _, need := needIDTypes[typ]; need {
				if missingOrEmpty(t, "id") {
					*out = append(*out, fmt.Sprintf("%s.id (type=%s)", path, typ))
				}
				if typ == "message" {
					if missingOrEmpty(t, "status") {
						*out = append(*out, path+".status")
					}
					if missingOrEmpty(t, "role") {
						*out = append(*out, path+".role")
					}
				}
				if typ == "function_call" && missingOrEmpty(t, "call_id") {
					*out = append(*out, path+".call_id")
				}
				if typ == "custom_tool_call" {
					if missingOrEmpty(t, "call_id") {
						*out = append(*out, path+".call_id")
					}
					if input, ok := t["input"]; !ok || input == nil {
						*out = append(*out, path+".input")
					}
					if missingOrEmpty(t, "name") {
						*out = append(*out, path+".name")
					}
				}
				if typ == "reasoning" {
					if summary, ok := t["summary"]; !ok || summary == nil {
						*out = append(*out, path+".summary")
					}
				}
				if typ == "web_search_call" {
					action, _ := t["action"].(map[string]any)
					if action == nil {
						*out = append(*out, path+".action")
					} else if actionType, _ := action["type"].(string); actionType == "" || actionType == "search" {
						if missingOrEmpty(action, "query") {
							*out = append(*out, path+".action.query")
						}
					}
				}
			}
			if contentPartTypes[typ] {
				if _, ok := t["annotations"]; !ok {
					*out = append(*out, path+".annotations")
				}
				if typ == "output_text" {
					if _, ok := t["logprobs"]; !ok {
						*out = append(*out, path+".logprobs")
					}
				}
			}
		}
		// message-like in output without id
		if role, _ := t["role"].(string); role == "assistant" || role == "user" {
			if _, hasContent := t["content"]; hasContent {
				if missingOrEmpty(t, "id") {
					*out = append(*out, path+".id (role="+role+")")
				}
				if arr, ok := t["content"].([]any); ok {
					for i, el := range arr {
						if part, ok := el.(map[string]any); ok {
							pt, _ := part["type"].(string)
							if pt == "output_text" || (pt == "" && hasText(part)) {
								if _, ok := part["annotations"]; !ok {
									*out = append(*out, fmt.Sprintf("%s.content[%d].annotations", path, i))
								}
							}
						}
					}
				}
			}
		}
		for k, child := range t {
			// avoid descending into tools definitions etc. heavily — still fine
			if k == "format" {
				continue
			}
			findMissingWalk(child, path+"."+k, out)
		}
	case []any:
		for i, child := range t {
			findMissingWalk(child, fmt.Sprintf("%s[%d]", path, i), out)
		}
	}
}

func isFormatConfig(m map[string]any) bool {
	// {"type":"text"} under format — not a content part
	typ, _ := m["type"].(string)
	if typ == "text" || typ == "json_schema" || typ == "json_object" {
		if _, hasAnn := m["annotations"]; !hasAnn {
			if _, hasText := m["text"]; !hasText {
				if _, hasFmt := m["format"]; !hasFmt {
					// bare format type object
					if len(m) <= 3 {
						return true
					}
				}
			}
		}
	}
	return false
}

func hasText(m map[string]any) bool {
	_, ok := m["text"]
	return ok
}

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return prefix + "_" + hex.EncodeToString(b[:])
}
