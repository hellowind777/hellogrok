package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

const maxResponsesHoldFrames = 32

var errResponsesHoldAbsorb = errors.New("withhold retryable responses failure")

// responsesHoldResult is returned when streamResponsesSSE has not written to
// the client. absorb asks the facade loop to treat the SSE as a retryable
// HTTP error; abort means the client left during the hold.
type responsesHoldResult struct {
	absorb     bool
	abort      bool
	status     int
	body       []byte
	retryAfter int
}

type responsesHoldAction int

const (
	responsesHoldBuffer responsesHoldAction = iota
	responsesHoldRelease
	responsesHoldAbsorb
)

func classifyResponsesHoldEvent(event map[string]any) responsesHoldAction {
	if event == nil {
		return responsesHoldBuffer
	}
	switch stringValue(event["type"]) {
	case "", "response.created", "response.in_progress", "response.queued":
		return responsesHoldBuffer
	case "response.failed", "error":
		if stringValue(event["code"]) == "proxy_stream_error" {
			return responsesHoldRelease
		}
		if _, _, _, ok := retryableResponsesStreamFailure(event); ok {
			return responsesHoldAbsorb
		}
		return responsesHoldRelease
	default:
		return responsesHoldRelease
	}
}

func retryableResponsesStreamFailure(event map[string]any) (status int, retryAfter int, body []byte, ok bool) {
	body = responsesFailureErrorJSON(event)
	if len(body) == 0 {
		return 0, 0, nil, false
	}
	classified, transient := classifyStructuredRetry(body)
	if !classified || !transient {
		return 0, 0, nil, false
	}
	return transientFailureStatus(body), responsesFailureRetryAfter(event), body, true
}

func shouldAbsorbResponsesFailure(event map[string]any, absorb *absorbState) bool {
	status, _, _, ok := retryableResponsesStreamFailure(event)
	if !ok || absorb == nil || absorb.budget <= 0 {
		return false
	}
	return absorbEligible(status, true, absorb.resilience)
}

func responsesFailureErrorJSON(event map[string]any) []byte {
	if event == nil {
		return nil
	}
	if errorBody, _ := event["error"].(map[string]any); errorBody != nil {
		encoded, err := json.Marshal(map[string]any{"error": errorBody})
		if err == nil {
			return encoded
		}
	}
	if response, _ := event["response"].(map[string]any); response != nil {
		if errorBody, _ := response["error"].(map[string]any); errorBody != nil {
			encoded, err := json.Marshal(map[string]any{"error": errorBody})
			if err == nil {
				return encoded
			}
		}
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return nil
	}
	return encoded
}

func responsesFailureRetryAfter(event map[string]any) int {
	var sources []map[string]any
	if errorBody, _ := event["error"].(map[string]any); errorBody != nil {
		sources = append(sources, errorBody)
	}
	if response, _ := event["response"].(map[string]any); response != nil {
		sources = append(sources, response)
		if errorBody, _ := response["error"].(map[string]any); errorBody != nil {
			sources = append(sources, errorBody)
		}
	}
	for _, src := range sources {
		if n := retryAfterFromAny(src["retry_after"]); n > 0 {
			return n
		}
	}
	return 0
}

func retryAfterFromAny(value any) int {
	switch typed := value.(type) {
	case json.Number:
		n, err := typed.Int64()
		if err != nil || n <= 0 || n > 1<<31-1 {
			return 0
		}
		return int(n)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(typed))
		if err != nil || n <= 0 {
			return 0
		}
		return n
	default:
		return 0
	}
}

func transientFailureStatus(body []byte) int {
	root, err := decodeJSONMap(body)
	if err != nil {
		return http.StatusServiceUnavailable
	}
	errorBody, _ := root["error"].(map[string]any)
	blob := strings.ToLower(strings.Join([]string{
		firstString(errorBody, "code"),
		firstString(errorBody, "type"),
		firstString(errorBody, "message"),
		firstString(root, "code"),
		firstString(root, "type"),
		firstString(root, "message"),
	}, " "))
	if strings.Contains(blob, "rate_limit") || strings.Contains(blob, "rate limit") ||
		strings.Contains(blob, "too_many") || strings.Contains(blob, "too many") ||
		strings.Contains(blob, "concurrency") {
		return http.StatusTooManyRequests
	}
	return http.StatusServiceUnavailable
}

func syntheticAbsorbResponse(status int, retryAfter int) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	if retryAfter > 0 {
		header.Set("Retry-After", strconv.Itoa(retryAfter))
	}
	return &http.Response{StatusCode: status, Header: header}
}
