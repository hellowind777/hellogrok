package proxy

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// usageCountKeys are the top-level token count fields a provider may report,
// across Chat Completions, Responses, and Messages dialects.
var usageCountKeys = []string{
	"prompt_tokens",
	"input_tokens",
	"completion_tokens",
	"output_tokens",
	"total_tokens",
	"prompt_cache_hit_tokens",
	"prompt_cache_miss_tokens",
	"cache_read_input_tokens",
	"cache_creation_input_tokens",
}

// usageDetailContainers maps a usage sub-object to the count keys it may
// carry. Covers the Chat Completions details pair and the Responses
// input/output details pair.
var usageDetailContainers = map[string][]string{
	"prompt_tokens_details":     {"cached_tokens", "audio_tokens"},
	"completion_tokens_details": {"reasoning_tokens", "audio_tokens", "accepted_prediction_tokens", "rejected_prediction_tokens"},
	"input_tokens_details":      {"cached_tokens"},
	"output_tokens_details":     {"reasoning_tokens"},
}

// rectifyUsageMap repairs a provider usage measurement in place so the core
// token counts survive transport quirks that would otherwise force Grok Build
// onto its byte-based estimate (and premature auto-compaction). Two defect
// classes are handled differently:
//
//   - Core counts (any key in usageCountKeys): numeric strings and whole
//     floats are coerced to integers; values that cannot be a count
//     (negative, fractional, overflowing, non-numeric) are deleted so the
//     downstream validator treats the measurement as incomplete and drops it,
//     rather than trusting a corrupt number.
//   - Optional decorations (detail containers, cost): an invalid entry is
//     removed without poisoning the core measurement. A relay that sends
//     "cost_in_usd_ticks": 0.0003 or a stringly-typed cached_tokens still has
//     a perfectly usable prompt/completion pair.
//
// Every repair is returned as a note so callers can keep the defect visible
// in the proxy log.
func rectifyUsageMap(usage map[string]any) []string {
	if usage == nil {
		return nil
	}
	var notes []string
	for _, key := range usageCountKeys {
		value, present := usage[key]
		if !present || value == nil {
			continue
		}
		count, ok := coerceUsageTokenCount(value)
		if !ok {
			delete(usage, key)
			notes = append(notes, fmt.Sprintf("dropped-invalid(%s)", key))
			continue
		}
		switch value.(type) {
		case int, int64, uint64:
			// Already an integer Go type; no repair worth logging.
		default:
			notes = append(notes, fmt.Sprintf("coerced(%s)", key))
		}
		usage[key] = count
	}
	for container, keys := range usageDetailContainers {
		raw, present := usage[container]
		if !present || raw == nil {
			continue
		}
		details, ok := raw.(map[string]any)
		if !ok {
			delete(usage, container)
			notes = append(notes, fmt.Sprintf("dropped-invalid(%s)", container))
			continue
		}
		for _, key := range keys {
			value, exists := details[key]
			if !exists {
				continue
			}
			if value == nil {
				delete(details, key)
				notes = append(notes, fmt.Sprintf("dropped-invalid(%s.%s)", container, key))
				continue
			}
			count, ok := coerceUsageTokenCount(value)
			if !ok {
				delete(details, key)
				notes = append(notes, fmt.Sprintf("dropped-invalid(%s.%s)", container, key))
				continue
			}
			switch value.(type) {
			case int, int64, uint64:
			default:
				notes = append(notes, fmt.Sprintf("coerced(%s.%s)", container, key))
			}
			details[key] = count
		}
	}
	if value, present := usage["cost_in_usd_ticks"]; present && value != nil && !validOptionalChatCostValue(value) {
		delete(usage, "cost_in_usd_ticks")
		notes = append(notes, "dropped-invalid(cost_in_usd_ticks)")
	}
	return notes
}

// rectifyResponsesUsageEnvelope rectifies the usage of a native Responses
// body, accepting both a bare response object and an SSE event envelope
// wrapping one. Mirrors guardResponsesUsage's envelope handling.
func rectifyResponsesUsageEnvelope(root map[string]any) []string {
	if root == nil {
		return nil
	}
	envelope := root
	if inner, ok := root["response"].(map[string]any); ok {
		envelope = inner
	}
	usage, _ := envelope["usage"].(map[string]any)
	if usage == nil {
		return nil
	}
	return rectifyUsageMap(usage)
}

// rectifyNativeMessagesUsage rectifies the usage of a native Messages body.
func rectifyNativeMessagesUsage(root map[string]any) []string {
	if root == nil {
		return nil
	}
	usage, _ := root["usage"].(map[string]any)
	if usage == nil {
		return nil
	}
	return rectifyUsageMap(usage)
}

// coerceUsageTokenCount accepts the integer representations relays actually
// emit (json.Number, whole float64, Go int types, decimal strings) and
// returns the count. Rejects negatives, fractions, overflow, and anything
// non-numeric.
func coerceUsageTokenCount(value any) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		if parsed, err := number.Int64(); err == nil {
			return parsed, parsed >= 0 && parsed <= maxCanonicalTokenCount
		}
		asFloat, err := strconv.ParseFloat(number.String(), 64)
		if err != nil || asFloat != math.Trunc(asFloat) {
			return 0, false
		}
		return coerceUsageTokenCount(asFloat)
	case float64:
		if number < 0 || number > float64(maxCanonicalTokenCount) || number != math.Trunc(number) {
			return 0, false
		}
		return int64(number), true
	case string:
		trimmed := strings.TrimSpace(number)
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return 0, false
		}
		return parsed, parsed >= 0 && parsed <= maxCanonicalTokenCount
	case int:
		return int64(number), number >= 0
	case int64:
		return number, number >= 0 && number <= maxCanonicalTokenCount
	case uint64:
		return int64(number), number <= uint64(maxCanonicalTokenCount)
	default:
		return 0, false
	}
}

func validOptionalChatCostValue(value any) bool {
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Int64()
		return err == nil && parsed >= 0
	case int:
		return number >= 0
	case int64:
		return number >= 0
	case uint64:
		return number <= uint64(^uint64(0)>>1)
	default:
		return false
	}
}
