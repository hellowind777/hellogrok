package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRectifyUsageMapCoercesTransportQuirks(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":            "12345",
		"completion_tokens":        678.0,
		"total_tokens":             json.Number("13023"),
		"prompt_cache_hit_tokens":  "100",
		"prompt_cache_miss_tokens": uint64(200),
	}
	notes := rectifyUsageMap(usage)
	for key, want := range map[string]int64{
		"prompt_tokens":            12345,
		"completion_tokens":        678,
		"total_tokens":             13023,
		"prompt_cache_hit_tokens":  100,
		"prompt_cache_miss_tokens": 200,
	} {
		got, present, valid := optionalCanonicalToken(usage, key)
		if !present || !valid || got != want {
			t.Fatalf("%s=%#v want %d (notes=%v)", key, usage[key], want, notes)
		}
	}
	for _, key := range []string{"prompt_tokens", "completion_tokens", "total_tokens", "prompt_cache_hit_tokens"} {
		if !containsNote(notes, "coerced("+key+")") {
			t.Fatalf("missing coercion note for %s: %v", key, notes)
		}
	}
	if containsNote(notes, "coerced(prompt_cache_miss_tokens)") {
		t.Fatalf("integer-typed value should not earn a coercion note: %v", notes)
	}
}

func TestRectifyUsageMapDropsUnrecoverableCoreCounts(t *testing.T) {
	tests := []struct {
		name  string
		value any
	}{
		{name: "fractional float", value: 1.5},
		{name: "negative", value: -1},
		{name: "negative string", value: "-1"},
		{name: "fractional string", value: "1.5"},
		{name: "non-numeric string", value: "unavailable"},
		{name: "overflow", value: uint64(maxCanonicalTokenCount) + 1},
		{name: "boolean", value: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			usage := map[string]any{"prompt_tokens": test.value, "completion_tokens": 3}
			notes := rectifyUsageMap(usage)
			if _, present := usage["prompt_tokens"]; present {
				t.Fatalf("unrecoverable count survived: %#v", usage)
			}
			if !containsNote(notes, "dropped-invalid(prompt_tokens)") {
				t.Fatalf("missing drop note: %v", notes)
			}
			if _, present := usage["completion_tokens"]; !present {
				t.Fatalf("healthy count was dropped: %#v", usage)
			}
		})
	}
}

func TestRectifyUsageMapRepairsDecorationsWithoutTouchingCore(t *testing.T) {
	usage := map[string]any{
		"prompt_tokens":         30,
		"completion_tokens":     5,
		"total_tokens":          35,
		"prompt_tokens_details": "bad",
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": "12",
			"audio_tokens":     nil,
		},
		"cost_in_usd_ticks": 0.0003,
	}
	notes := rectifyUsageMap(usage)
	if _, present := usage["prompt_tokens_details"]; present {
		t.Fatalf("non-map details container survived: %#v", usage)
	}
	details, _ := usage["completion_tokens_details"].(map[string]any)
	if cached, present, valid := optionalCanonicalToken(details, "reasoning_tokens"); !present || !valid || cached != 12 {
		t.Fatalf("string reasoning_tokens was not coerced: %#v", details)
	}
	if _, present := details["audio_tokens"]; present {
		t.Fatalf("nil detail value survived: %#v", details)
	}
	if _, present := usage["cost_in_usd_ticks"]; present {
		t.Fatalf("invalid cost survived: %#v", usage)
	}
	prompt, _, _ := optionalCanonicalToken(usage, "prompt_tokens")
	total, _, _ := optionalCanonicalToken(usage, "total_tokens")
	if prompt != 30 || total != 35 {
		t.Fatalf("core measurement changed: %#v", usage)
	}
	for _, want := range []string{
		"dropped-invalid(prompt_tokens_details)",
		"coerced(completion_tokens_details.reasoning_tokens)",
		"dropped-invalid(completion_tokens_details.audio_tokens)",
		"dropped-invalid(cost_in_usd_ticks)",
	} {
		if !containsNote(notes, want) {
			t.Fatalf("missing note %q: %v", want, notes)
		}
	}
}

func TestRectifyUsageMapNilAndEmptyAreNoops(t *testing.T) {
	if notes := rectifyUsageMap(nil); len(notes) != 0 {
		t.Fatalf("nil usage produced notes: %v", notes)
	}
	if notes := rectifyUsageMap(map[string]any{}); len(notes) != 0 {
		t.Fatalf("empty usage produced notes: %v", notes)
	}
	if notes := rectifyResponsesUsageEnvelope(map[string]any{}); len(notes) != 0 {
		t.Fatalf("usage-less envelope produced notes: %v", notes)
	}
}

func TestRectifyResponsesUsageEnvelopeFindsBareAndWrappedUsage(t *testing.T) {
	bare := map[string]any{"usage": map[string]any{"input_tokens": "30", "output_tokens": 5}}
	if notes := rectifyResponsesUsageEnvelope(bare); !containsNote(notes, "coerced(input_tokens)") {
		t.Fatalf("bare usage was not rectified: %v", notes)
	}
	wrapped := map[string]any{"response": map[string]any{"usage": map[string]any{"input_tokens": "30", "output_tokens": 5}}}
	if notes := rectifyResponsesUsageEnvelope(wrapped); !containsNote(notes, "coerced(input_tokens)") {
		t.Fatalf("wrapped usage was not rectified: %v", notes)
	}
	inner, _ := wrapped["response"].(map[string]any)
	usage, _ := inner["usage"].(map[string]any)
	if got, _, _ := optionalCanonicalToken(usage, "input_tokens"); got != 30 {
		t.Fatalf("wrapped usage was not mutated in place: %#v", usage)
	}
}

// End-to-end through the native Chat normalizer: a stringly-typed usage that
// previously forced Grok Build onto its byte estimate now reaches it intact.
func TestNormalizeNativeChatUsageRectifiesStringCounts(t *testing.T) {
	root := map[string]any{"usage": map[string]any{
		"prompt_tokens": "30", "completion_tokens": "5", "total_tokens": "35",
	}}
	notes := normalizeNativeChatUsage(root, 0)
	usage, ok := root["usage"].(map[string]any)
	if !ok {
		t.Fatalf("rectifiable usage was dropped: %#v (notes=%v)", root["usage"], notes)
	}
	prompt, _, _ := optionalCanonicalToken(usage, "prompt_tokens")
	total, _, _ := optionalCanonicalToken(usage, "total_tokens")
	if prompt != 30 || total != 35 {
		t.Fatalf("rectified usage=%#v want prompt=30 total=35", usage)
	}
}

// The core measurement stays droppable: rectification must not rescue a usage
// whose prompt count is genuinely unusable.
func TestNormalizeNativeChatUsageStillDropsUnusableCore(t *testing.T) {
	root := map[string]any{"usage": map[string]any{
		"prompt_tokens": "unavailable", "completion_tokens": 5, "total_tokens": 35,
	}}
	notes := normalizeNativeChatUsage(root, 0)
	if root["usage"] != nil {
		t.Fatalf("unusable core measurement survived: %#v", root["usage"])
	}
	if !containsNote(notes, "dropped-invalid(prompt_tokens)") ||
		!containsNote(notes, "usage-dropped(core-measurement-unusable)") {
		t.Fatalf("missing drop notes: %v", notes)
	}
}

func TestTranslatedUsageRectifiesStringCounts(t *testing.T) {
	var result canonicalResult
	applyChatUsage(&result, map[string]any{
		"prompt_tokens": "30", "completion_tokens": "5", "total_tokens": "35",
	})
	if !result.UsagePresent || result.InputTokens != 30 || result.OutputTokens != 5 || result.TotalTokens != 35 {
		t.Fatalf("stringly-typed translated usage was not rectified: %#v", result)
	}
	var messagesResult canonicalResult
	applyMessagesUsage(&messagesResult, map[string]any{
		"input_tokens": "30", "output_tokens": "5",
	})
	if !messagesResult.UsagePresent || messagesResult.InputTokens != 30 || messagesResult.OutputTokens != 5 {
		t.Fatalf("stringly-typed messages usage was not rectified: %#v", messagesResult)
	}
}

func containsNote(notes []string, want string) bool {
	for _, note := range notes {
		if strings.Contains(note, want) {
			return true
		}
	}
	return false
}
