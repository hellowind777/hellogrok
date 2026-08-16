package proxy

import (
	"bytes"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
)

var exactContextLengthMessage = regexp.MustCompile(
	`(?i)^this model's maximum context length is ([0-9][0-9,]*) tokens\. however, you requested ([0-9][0-9,]*) tokens \(([0-9][0-9,]*) in the messages, ([0-9][0-9,]*) in the completion\)\. please reduce the length of the messages or completion\.$`,
)

var semanticContextLengthMessage = regexp.MustCompile(
	`(?i)\bmaximum context (?:length|window)(?:\s+is|\s*=|:)\s*([0-9][0-9,]*)\s+tokens\b`,
)

var semanticContextWindowKeys = map[string]struct{}{
	"context_window":         {},
	"max_context_tokens":     {},
	"maximum_context_length": {},
	"maximum_context_tokens": {},
	"model_context_window":   {},
	"max_model_len":          {},
}

type contextBudgetObservation struct {
	MaximumTokens    uint64
	RequestedTokens  uint64
	MessageTokens    uint64
	CompletionTokens uint64
	ExactRequest     bool
}

type contextBudgetRetry struct {
	Body            []byte
	MaximumTokens   uint64
	MessageTokens   uint64
	OriginalOutput  uint64
	AvailableOutput uint64
}

// inspectContextBudgetError discovers provider capacity independently from
// retry eligibility. This lets Grok Build learn the real window and compact on
// the next prompt even when the current request has no output budget left.
func inspectContextBudgetError(status int, body []byte) (contextBudgetObservation, bool) {
	if status < 400 || status >= 500 {
		return contextBudgetObservation{}, false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return contextBudgetObservation{}, false
	}

	stringsInError := make([]string, 0, 2)
	semanticValues := make(map[uint64]struct{})
	collectContextBudgetValues(root, 0, semanticValues, &stringsInError)
	exactObservations := make([]contextBudgetObservation, 0, 1)
	for _, text := range stringsInError {
		if observation, ok := exactContextBudgetObservation(text); ok {
			exactObservations = append(exactObservations, observation)
			semanticValues[observation.MaximumTokens] = struct{}{}
		}
	}
	for _, text := range stringsInError {
		match := semanticContextLengthMessage.FindStringSubmatch(strings.TrimSpace(text))
		if match == nil {
			continue
		}
		if value, ok := positiveDecimalUint64(match[1]); ok {
			semanticValues[value] = struct{}{}
		}
	}
	if len(semanticValues) != 1 {
		return contextBudgetObservation{}, false
	}
	if len(exactObservations) > 0 {
		observation := exactObservations[0]
		for _, other := range exactObservations[1:] {
			if other != observation {
				return contextBudgetObservation{}, false
			}
		}
		observation.ExactRequest = status == 400 && invalidRequestErrorType(root)
		return observation, true
	}
	for value := range semanticValues {
		return contextBudgetObservation{MaximumTokens: value}, true
	}
	return contextBudgetObservation{}, false
}

func invalidRequestErrorType(root any) bool {
	object, ok := root.(map[string]any)
	if !ok {
		return false
	}
	if nested, ok := object["error"].(map[string]any); ok {
		object = nested
	}
	return strings.EqualFold(strings.TrimSpace(stringValue(object["type"])), "invalid_request_error")
}

func collectContextBudgetValues(value any, depth int, values map[uint64]struct{}, messages *[]string) {
	if depth > 12 {
		return
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalizedKey := strings.ToLower(strings.TrimSpace(key))
			if _, ok := semanticContextWindowKeys[normalizedKey]; ok {
				if parsed, ok := positiveJSONUint64(child); ok {
					values[parsed] = struct{}{}
				}
			}
			if normalizedKey == "message" || normalizedKey == "detail" || normalizedKey == "error_description" {
				if text, ok := child.(string); ok {
					*messages = append(*messages, text)
				}
			}
			collectContextBudgetValues(child, depth+1, values, messages)
		}
	case []any:
		for _, child := range typed {
			collectContextBudgetValues(child, depth+1, values, messages)
		}
	}
}

func exactContextBudgetObservation(message string) (contextBudgetObservation, bool) {
	match := exactContextLengthMessage.FindStringSubmatch(strings.TrimSpace(message))
	if match == nil {
		return contextBudgetObservation{}, false
	}
	values := make([]uint64, 4)
	for index := range values {
		parsed, ok := positiveDecimalUint64(match[index+1])
		if !ok {
			return contextBudgetObservation{}, false
		}
		values[index] = parsed
	}
	maximum, requested, messages, completion := values[0], values[1], values[2], values[3]
	if requested <= maximum || messages > ^uint64(0)-completion || requested != messages+completion {
		return contextBudgetObservation{}, false
	}
	return contextBudgetObservation{
		MaximumTokens: maximum, RequestedTokens: requested,
		MessageTokens: messages, CompletionTokens: completion, ExactRequest: true,
	}, true
}

func positiveDecimalUint64(value string) (uint64, bool) {
	parsed, err := strconv.ParseUint(strings.ReplaceAll(strings.TrimSpace(value), ",", ""), 10, 64)
	return parsed, err == nil && parsed > 0
}

// clampCompletionForContextError changes only the output allowance proven by
// an exact, internally consistent provider rejection. Other discoveries still
// reach Grok Build as capacity metadata but do not trigger a speculative retry.
func clampCompletionForContextError(observation contextBudgetObservation, requestBody []byte, protocol wireProtocol) (contextBudgetRetry, bool) {
	if !observation.ExactRequest || observation.MessageTokens >= observation.MaximumTokens {
		return contextBudgetRetry{}, false
	}

	root, err := decodeRequestObject(requestBody)
	if err != nil {
		return contextBudgetRetry{}, false
	}
	key := "max_tokens"
	if protocol == wireResponses {
		key = "max_output_tokens"
	}
	actual, ok := positiveJSONUint64(root[key])
	if !ok || actual != observation.CompletionTokens {
		return contextBudgetRetry{}, false
	}
	available := observation.MaximumTokens - observation.MessageTokens
	if available == 0 || available >= actual {
		return contextBudgetRetry{}, false
	}
	root[key] = available
	encoded, err := encodeRequestObject(root)
	if err != nil {
		return contextBudgetRetry{}, false
	}
	return contextBudgetRetry{
		Body: encoded, MaximumTokens: observation.MaximumTokens,
		MessageTokens: observation.MessageTokens, OriginalOutput: actual,
		AvailableOutput: available,
	}, true
}

func positiveJSONUint64(value any) (uint64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := strconv.ParseUint(number.String(), 10, 64)
		return parsed, err == nil && parsed > 0
	case string:
		return positiveDecimalUint64(number)
	case float64:
		if number <= 0 || number >= math.Exp2(64) || number != math.Trunc(number) {
			return 0, false
		}
		return uint64(number), true
	case int:
		return uint64(number), number > 0
	case int64:
		return uint64(number), number > 0
	case uint64:
		return number, number > 0
	default:
		return 0, false
	}
}

func setCompletionLimit(body []byte, protocol wireProtocol, limit uint64) ([]byte, bool) {
	if limit == 0 {
		return nil, false
	}
	root, err := decodeRequestObject(body)
	if err != nil {
		return nil, false
	}
	key := "max_tokens"
	if protocol == wireResponses {
		key = "max_output_tokens"
	}
	if _, ok := positiveJSONUint64(root[key]); !ok {
		return nil, false
	}
	root[key] = limit
	encoded, err := encodeRequestObject(root)
	return encoded, err == nil
}
