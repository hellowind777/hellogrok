package patch

import (
	"encoding/json"
	"strings"
	"testing"
)

// Congee stream: response.completed omits output[].id/status and content annotations.
func TestCompletedOutputMissingFields(t *testing.T) {
	raw := `{
		"type":"response.completed",
		"response":{
			"id":"resp_abc",
			"object":"response",
			"status":"completed",
			"model":"gpt-5.6-sol",
			"output":[
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}
			],
			"usage":{"input_tokens":1,"output_tokens":1}
		}
	}`
	// diagnose before patch
	miss := FindMissingJSON([]byte(raw))
	joined := strings.Join(miss, " ")
	if !strings.Contains(joined, "id") || !strings.Contains(joined, "annotations") {
		t.Fatalf("expected missing id/annotations, got %v", miss)
	}

	out := PatchJSONBytes([]byte(raw), Options{GPTResponses: true, WebSearch: true, RequestModel: "gpt-5.6-sol"})
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	resp := v["response"].(map[string]any)
	if resp["id"] != "resp_abc" {
		t.Fatalf("response id: %v", resp["id"])
	}
	arr := resp["output"].([]any)
	item := arr[0].(map[string]any)
	id, _ := item["id"].(string)
	if id == "" {
		t.Fatalf("output[0].id missing after patch: %s", string(out))
	}
	if item["status"] != "completed" {
		t.Fatalf("status: %v", item["status"])
	}
	content := item["content"].([]any)
	part := content[0].(map[string]any)
	ann, ok := part["annotations"].([]any)
	if !ok {
		t.Fatalf("annotations missing/wrong: %s", string(out))
	}
	if ann == nil {
		t.Fatal("annotations nil")
	}
	if _, ok := part["logprobs"]; !ok {
		t.Fatalf("logprobs missing: %s", string(out))
	}
	usage := resp["usage"].(map[string]any)
	if _, ok := usage["output_tokens_details"]; !ok {
		t.Fatalf("usage details missing: %s", string(out))
	}
	// after patch, critical fields should be gone
	if miss2 := FindMissingJSON(out); len(miss2) > 0 {
		t.Fatalf("still missing after patch: %v", miss2)
	}
}

func TestResponseCreatedAtIsRequiredAndPreserved(t *testing.T) {
	raw := `{"response":{"id":"resp_1","object":"response","status":"completed","output":[]}}`
	if miss := strings.Join(FindMissingJSON([]byte(raw)), " "); !strings.Contains(miss, "created_at") {
		t.Fatalf("created_at must be diagnosed before patch: %s", miss)
	}
	out := PatchJSONBytes([]byte(raw), Options{GPTResponses: true})
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	resp := v["response"].(map[string]any)
	if created, ok := resp["created_at"].(float64); !ok || created <= 0 {
		t.Fatalf("created_at missing after patch: %s", out)
	}

	existing := `{"id":"resp_2","object":"response","status":"completed","created_at":123,"output":[]}`
	kept := PatchJSONBytes([]byte(existing), Options{GPTResponses: true})
	var keptValue map[string]any
	_ = json.Unmarshal(kept, &keptValue)
	if keptValue["created_at"].(float64) != 123 {
		t.Fatalf("existing created_at changed: %s", kept)
	}
}

func TestHostedResponseToolChoiceIsCompatibleWithBuildParser(t *testing.T) {
	for _, choice := range []string{"web_search", "web_search_2025_08_26", "x_search"} {
		t.Run(choice, func(t *testing.T) {
			raw := `{"id":"resp_1","object":"response","created_at":123,"status":"completed","model":"deepseek-v4-flash","output":[],"tool_choice":{"type":"` + choice + `"}}`
			patched := PatchJSONBytes([]byte(raw), Options{GPTResponses: true, WebSearch: true})
			var response map[string]any
			if err := json.Unmarshal(patched, &response); err != nil {
				t.Fatal(err)
			}
			if response["tool_choice"] != "auto" {
				t.Fatalf("hosted response tool_choice was not normalized: %s", patched)
			}
		})
	}

	function := `{"id":"resp_2","object":"response","created_at":123,"status":"completed","model":"model","output":[],"tool_choice":{"type":"function","name":"lookup"}}`
	patched := PatchJSONBytes([]byte(function), Options{GPTResponses: true})
	if !strings.Contains(string(patched), `"tool_choice":{"name":"lookup","type":"function"}`) &&
		!strings.Contains(string(patched), `"tool_choice":{"type":"function","name":"lookup"}`) {
		t.Fatalf("ordinary function choice changed: %s", patched)
	}
}

func TestResponseEventSequenceNumberIsRequiredAndPreserved(t *testing.T) {
	missing := `{"type":"response.created","response":{"id":"resp_1","object":"response","created_at":123,"status":"in_progress","output":[]}}`
	if miss := strings.Join(FindMissingJSON([]byte(missing)), " "); !strings.Contains(miss, "sequence_number") {
		t.Fatalf("sequence_number must be diagnosed before patch: %s", miss)
	}
	patched := PatchJSONBytes([]byte(missing), Options{GPTResponses: true})
	var event map[string]any
	if err := json.Unmarshal(patched, &event); err != nil {
		t.Fatal(err)
	}
	if got, ok := event["sequence_number"].(float64); !ok || got != 0 {
		t.Fatalf("missing response.created sequence must become zero: %s", patched)
	}

	existing := `{"type":"response.completed","sequence_number":9,"response":{"id":"resp_1","object":"response","created_at":123,"status":"completed","output":[]}}`
	kept := PatchJSONBytes([]byte(existing), Options{GPTResponses: true})
	if err := json.Unmarshal(kept, &event); err != nil {
		t.Fatal(err)
	}
	if event["sequence_number"].(float64) != 9 {
		t.Fatalf("existing sequence_number changed: %s", kept)
	}
}

// Must NOT inject annotations into response.text.format {type:text}
func TestDoNotPatchFormatTypeText(t *testing.T) {
	raw := `{
		"type":"response.created",
		"response":{
			"id":"resp_1",
			"object":"response",
			"status":"in_progress",
			"output":[],
			"text":{"format":{"type":"text"},"verbosity":"medium"}
		}
	}`
	out := PatchJSONBytes([]byte(raw), Options{GPTResponses: true})
	var v map[string]any
	_ = json.Unmarshal(out, &v)
	resp := v["response"].(map[string]any)
	text := resp["text"].(map[string]any)
	format := text["format"].(map[string]any)
	if _, ok := format["annotations"]; ok {
		t.Fatalf("must not add annotations to format: %s", string(out))
	}
	if _, ok := format["text"]; ok {
		t.Fatalf("must not add text field to format: %s", string(out))
	}
}

func TestSSEDataLine(t *testing.T) {
	line := `data: {"type":"response.completed","response":{"id":"resp_1","object":"response","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"x"}]}]}}`
	out := PatchSSEDataLine(line, Options{GPTResponses: true, WebSearch: true})
	var payload string
	if strings.HasPrefix(out, "data:") {
		payload = strings.TrimSpace(strings.TrimPrefix(out, "data:"))
	}
	var v map[string]any
	_ = json.Unmarshal([]byte(payload), &v)
	resp := v["response"].(map[string]any)
	item := resp["output"].([]any)[0].(map[string]any)
	if s, _ := item["id"].(string); s == "" {
		t.Fatalf("no id on output item: %s", out)
	}
	part := item["content"].([]any)[0].(map[string]any)
	if _, ok := part["annotations"]; !ok {
		t.Fatalf("no annotations: %s", out)
	}
}

func TestSSEDataLineUsesFallbackSequenceAndPreservesExisting(t *testing.T) {
	missing := `data: {"type":"response.output_text.delta","delta":"x"}`
	patched := PatchSSEDataLineWithSequence(missing, Options{}, 7)
	var event map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(patched, "data:"))), &event); err != nil {
		t.Fatal(err)
	}
	if event["sequence_number"].(float64) != 7 {
		t.Fatalf("fallback sequence not used: %s", patched)
	}

	existing := `data: {"type":"response.output_text.delta","sequence_number":9,"delta":"x"}`
	kept := PatchSSEDataLineWithSequence(existing, Options{}, 7)
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(kept, "data:"))), &event); err != nil {
		t.Fatal(err)
	}
	if event["sequence_number"].(float64) != 9 {
		t.Fatalf("existing sequence changed: %s", kept)
	}
}

func TestSSETerminalUsageKeepsUnknownMeasurementsNull(t *testing.T) {
	tests := []struct {
		name  string
		usage string
	}{
		{name: "missing", usage: ""},
		{name: "null", usage: `,"usage":null`},
		{name: "empty", usage: `,"usage":{}`},
		{name: "partial", usage: `,"usage":{"input_tokens":3}`},
		{name: "negative", usage: `,"usage":{"input_tokens":3,"output_tokens":-1}`},
		{name: "fractional", usage: `,"usage":{"input_tokens":3.5,"output_tokens":1}`},
		{name: "overflow", usage: `,"usage":{"total_tokens":4294967296}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			line := `data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"deepseek-v4-pro","output":[]` + test.usage + `}}`
			patched := PatchSSEDataLineWithSequence(line, Options{}, 9)
			var event map[string]any
			payload := strings.TrimSpace(strings.TrimPrefix(patched, "data:"))
			if err := json.Unmarshal([]byte(payload), &event); err != nil {
				t.Fatal(err)
			}
			response := event["response"].(map[string]any)
			usage, exists := response["usage"]
			if !exists || usage != nil {
				t.Fatalf("untrustworthy usage was not kept unknown: %s", patched)
			}
			if event["sequence_number"].(float64) != 9 {
				t.Fatalf("fallback sequence missing: %s", patched)
			}
		})
	}
}

func TestSSETerminalUsagePreservesExplicitZeroAndContextDetails(t *testing.T) {
	line := `data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"deepseek-v4-pro","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0,"context_details":{"input_tokens":40,"output_tokens":2}}}}`
	patched := PatchSSEDataLineWithSequence(line, Options{}, 1)
	var event map[string]any
	payload := strings.TrimSpace(strings.TrimPrefix(patched, "data:"))
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		t.Fatal(err)
	}
	usage := event["response"].(map[string]any)["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 0 {
		t.Fatalf("explicit zero measurement changed: %s", patched)
	}
	contextDetails := usage["context_details"].(map[string]any)
	if contextDetails["input_tokens"].(float64) != 40 || contextDetails["output_tokens"].(float64) != 2 {
		t.Fatalf("provider context_details changed: %s", patched)
	}
}

func TestPreserveExistingAnnotations(t *testing.T) {
	raw := `{
		"response":{
			"id":"resp_1","object":"response","status":"completed",
			"output":[{"type":"message","id":"msg_keep","status":"completed","role":"assistant",
				"content":[{"type":"output_text","text":"hi","annotations":[{"type":"url_citation"}]}]}],
			"usage":{"input_tokens":1,"output_tokens":1,
				"output_tokens_details":{"reasoning_tokens":3},
				"input_tokens_details":{"cached_tokens":2}}
		}
	}`
	out := PatchJSONBytes([]byte(raw), Options{GPTResponses: true, WebSearch: true})
	var v map[string]any
	_ = json.Unmarshal(out, &v)
	resp := v["response"].(map[string]any)
	item := resp["output"].([]any)[0].(map[string]any)
	if item["id"] != "msg_keep" {
		t.Fatalf("must not rewrite existing id: %v", item["id"])
	}
	part := item["content"].([]any)[0].(map[string]any)
	ann := part["annotations"].([]any)
	if len(ann) != 1 {
		t.Fatalf("must preserve annotations: %v", ann)
	}
	usage := resp["usage"].(map[string]any)
	otd := usage["output_tokens_details"].(map[string]any)
	if otd["reasoning_tokens"].(float64) != 3 {
		t.Fatalf("must preserve reasoning_tokens: %v", otd)
	}
}

func TestGatewayMissingOnlyUsageDetails(t *testing.T) {
	// Some gateways have full message schema but omit usage details (web_search path).
	raw := `{
		"id":"resp_x","object":"response","status":"completed","model":"deepseek-v4-flash",
		"output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant",
			"content":[{"type":"output_text","text":"search done","annotations":[]}]}],
		"usage":{"input_tokens":10,"output_tokens":5}
	}`
	out := PatchJSONBytes([]byte(raw), Options{GPTResponses: true, WebSearch: true})
	var v map[string]any
	_ = json.Unmarshal(out, &v)
	usage := v["usage"].(map[string]any)
	otd, ok := usage["output_tokens_details"].(map[string]any)
	if !ok {
		t.Fatal(string(out))
	}
	if _, ok := otd["reasoning_tokens"]; !ok {
		t.Fatal(string(out))
	}
	itd := usage["input_tokens_details"].(map[string]any)
	if _, ok := itd["cached_tokens"]; !ok {
		t.Fatal(string(out))
	}
	// existing message id untouched
	item := v["output"].([]any)[0].(map[string]any)
	if item["id"] != "msg_1" {
		t.Fatal(item["id"])
	}
}

func TestEmptyFinishReasonNull(t *testing.T) {
	raw := `{"choices":[{"finish_reason":"","message":{"role":"assistant","content":"x"}}]}`
	out := PatchJSONBytes([]byte(raw), Options{})
	if !strings.Contains(string(out), `"finish_reason":null`) {
		t.Fatal(string(out))
	}
}

func TestFunctionCallCallID(t *testing.T) {
	raw := `{"type":"response.output_item.done","item":{"type":"function_call","name":"x","arguments":"{}"}}`
	out := PatchJSONBytes([]byte(raw), Options{GPTResponses: true})
	var v map[string]any
	_ = json.Unmarshal(out, &v)
	item := v["item"].(map[string]any)
	if item["id"] == nil || item["id"] == "" {
		t.Fatal("id", string(out))
	}
	if item["call_id"] == nil || item["call_id"] == "" {
		t.Fatal("call_id", string(out))
	}
}

func TestNullAnnotationsBecomeEmpty(t *testing.T) {
	raw := `{"type":"output_text","text":"hi","annotations":null}`
	out := PatchJSONBytes([]byte(raw), Options{GPTResponses: true})
	var v map[string]any
	_ = json.Unmarshal(out, &v)
	ann, ok := v["annotations"].([]any)
	if !ok || ann == nil {
		t.Fatalf("want [] got %v in %s", v["annotations"], string(out))
	}
}

func TestWebSearchCallMissingActionGetsMinimalResponsesShape(t *testing.T) {
	raw := `{"type":"response.output_item.added","sequence_number":3,"item":{"type":"web_search_call","id":"ws_1","status":"in_progress"}}`
	missing := FindMissingJSON([]byte(raw))
	if !strings.Contains(strings.Join(missing, "\n"), ".action") {
		t.Fatalf("missing action was not diagnosed: %v", missing)
	}

	patched := PatchJSONBytes([]byte(raw), Options{GPTResponses: true, WebSearch: true})
	var event map[string]any
	if err := json.Unmarshal(patched, &event); err != nil {
		t.Fatal(err)
	}
	item := event["item"].(map[string]any)
	action := item["action"].(map[string]any)
	if action["type"] != "search" || action["query"] != "" {
		t.Fatalf("invalid fallback action: %s", patched)
	}
	if sources, ok := action["sources"].([]any); !ok || sources == nil || len(sources) != 0 {
		t.Fatalf("invalid fallback sources: %s", patched)
	}
}

func TestWebSearchCallPreservesExistingAction(t *testing.T) {
	raw := `{"type":"web_search_call","id":"ws_2","status":"completed","action":{"type":"search","query":"Grok Build settings","sources":[{"type":"url","url":"https://example.test"}]}}`
	patched := PatchJSONBytes([]byte(raw), Options{GPTResponses: true, WebSearch: true})
	var item map[string]any
	if err := json.Unmarshal(patched, &item); err != nil {
		t.Fatal(err)
	}
	action := item["action"].(map[string]any)
	if action["query"] != "Grok Build settings" {
		t.Fatalf("existing action was overwritten: %s", patched)
	}
	sources := action["sources"].([]any)
	if len(sources) != 1 || sources[0].(map[string]any)["url"] != "https://example.test" {
		t.Fatalf("existing sources were overwritten: %s", patched)
	}
}

func TestWebSearchCallPromotesProviderQueries(t *testing.T) {
	raw := `{"type":"web_search_call","id":"ws_2","status":"completed","action":{"type":"search","query":"","queries":["DeepSeek news","DeepSeek latest news","ws_call_id=ws_2"],"sources":[]}}`
	patched := PatchJSONBytes([]byte(raw), Options{GPTResponses: true, WebSearch: true})
	var item map[string]any
	if err := json.Unmarshal(patched, &item); err != nil {
		t.Fatal(err)
	}
	action := item["action"].(map[string]any)
	if action["query"] != "DeepSeek news" {
		t.Fatalf("provider queries were not promoted: %s", patched)
	}
	queries := action["queries"].([]any)
	if len(queries) != 3 {
		t.Fatalf("provider queries were modified: %s", patched)
	}
}

func TestWebSearchCallFillsMissingQueryInsideAction(t *testing.T) {
	raw := `{"type":"response.output_item.added","item":{"type":"web_search_call","id":"ws_3","status":"in_progress","action":{"type":"search"}}}`
	missing := strings.Join(FindMissingJSON([]byte(raw)), "\n")
	if !strings.Contains(missing, ".action.query") {
		t.Fatalf("missing query was not diagnosed: %s", missing)
	}

	patched := PatchJSONBytes([]byte(raw), Options{GPTResponses: true, WebSearch: true})
	var event map[string]any
	if err := json.Unmarshal(patched, &event); err != nil {
		t.Fatal(err)
	}
	action := event["item"].(map[string]any)["action"].(map[string]any)
	if action["query"] != "" {
		t.Fatalf("missing query was not filled: %s", patched)
	}
	if sources, ok := action["sources"].([]any); !ok || sources == nil {
		t.Fatalf("missing sources was not filled: %s", patched)
	}
}

func TestCustomToolCallUsesBuildSchema(t *testing.T) {
	raw := `{"type":"response.output_item.done","item":{"type":"custom_tool_call","arguments":{"query":"latest"}}}`
	missing := strings.Join(FindMissingJSON([]byte(raw)), "\n")
	for _, field := range []string{".call_id", ".input", ".name"} {
		if !strings.Contains(missing, field) {
			t.Fatalf("missing %s was not diagnosed: %s", field, missing)
		}
	}

	patched := PatchJSONBytes([]byte(raw), Options{GPTResponses: true})
	var event map[string]any
	if err := json.Unmarshal(patched, &event); err != nil {
		t.Fatal(err)
	}
	item := event["item"].(map[string]any)
	if item["id"] == "" || item["call_id"] == "" || item["name"] != "x_search" {
		t.Fatalf("required custom tool fields were not completed: %s", patched)
	}
	if item["input"] != `{"query":"latest"}` {
		t.Fatalf("arguments were not converted to custom-tool input: %s", patched)
	}
	if _, added := item["status"]; added {
		t.Fatalf("custom_tool_call has no status field in Build's schema: %s", patched)
	}
}

func TestInputTextDoesNotReceiveOutputAnnotations(t *testing.T) {
	raw := `{"type":"input_text","text":"hi"}`
	patched := PatchJSONBytes([]byte(raw), Options{GPTResponses: true})
	var part map[string]any
	if err := json.Unmarshal(patched, &part); err != nil {
		t.Fatal(err)
	}
	if _, exists := part["annotations"]; exists {
		t.Fatalf("input_text was given an output-only field: %s", patched)
	}
}

func TestReasoningNullSummaryAndIncompleteUsageAreCompleted(t *testing.T) {
	raw := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"type":"reasoning","id":"rs_1","summary":null}],"usage":{"input_tokens":2,"output_tokens":3}}`
	patched := PatchJSONBytes([]byte(raw), Options{GPTResponses: true})
	var response map[string]any
	if err := json.Unmarshal(patched, &response); err != nil {
		t.Fatal(err)
	}
	reasoning := response["output"].([]any)[0].(map[string]any)
	if summary, ok := reasoning["summary"].([]any); !ok || summary == nil {
		t.Fatalf("reasoning summary is not a JSON array: %s", patched)
	}
	usage := response["usage"].(map[string]any)
	if usage["total_tokens"] != float64(5) {
		t.Fatalf("total_tokens was not derived: %s", patched)
	}
}

func TestMissingOrEmptyUsageDoesNotBecomeZero(t *testing.T) {
	for _, raw := range []string{
		`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[]}`,
		`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[],"usage":null}`,
		`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[],"usage":{}}`,
	} {
		patched := PatchJSONBytes([]byte(raw), Options{GPTResponses: true})
		var response map[string]any
		if err := json.Unmarshal(patched, &response); err != nil {
			t.Fatal(err)
		}
		if usage, exists := response["usage"]; !exists || usage != nil {
			t.Fatalf("missing usage was not preserved as null: %s", patched)
		}
	}
}

func TestAllZeroUsageRemainsUnknown(t *testing.T) {
	raw := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`
	patched := PatchJSONBytes([]byte(raw), Options{GPTResponses: true})
	var response map[string]any
	if err := json.Unmarshal(patched, &response); err != nil {
		t.Fatal(err)
	}
	if response["usage"] != nil {
		t.Fatalf("all-zero placeholder was forwarded: %s", patched)
	}
}

func TestInvalidOrPartialUsageRemainsUnknown(t *testing.T) {
	tests := map[string]string{
		"input only":               `{"input_tokens":3}`,
		"output only":              `{"output_tokens":3}`,
		"total only":               `{"total_tokens":7}`,
		"input and total only":     `{"input_tokens":3,"total_tokens":7}`,
		"negative output":          `{"input_tokens":3,"output_tokens":-1}`,
		"fractional output":        `{"input_tokens":3,"output_tokens":1.5}`,
		"wire count overflow":      `{"input_tokens":4294967296,"output_tokens":1}`,
		"derived total overflow":   `{"input_tokens":4294967295,"output_tokens":1}`,
		"invalid reasoning detail": `{"input_tokens":3,"output_tokens":1,"output_tokens_details":{"reasoning_tokens":-1}}`,
		"non-object details":       `{"input_tokens":3,"output_tokens":1,"input_tokens_details":7}`,
	}
	for name, usage := range tests {
		t.Run(name, func(t *testing.T) {
			raw := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[],"usage":` + usage + `}`
			patched := PatchJSONBytes([]byte(raw), Options{GPTResponses: true})
			var response map[string]any
			if err := json.Unmarshal(patched, &response); err != nil {
				t.Fatal(err)
			}
			if response["usage"] != nil {
				t.Fatalf("untrustworthy usage was forwarded: %s", patched)
			}
		})
	}
}

func TestContextDetailsAreDerivedOnlyFromCompleteRealUsage(t *testing.T) {
	tests := []struct {
		name        string
		usage       string
		wantContext bool
		wantNull    bool
	}{
		{name: "complete", usage: `{"input_tokens":40,"output_tokens":2,"total_tokens":99}`, wantContext: true},
		{name: "all-zero placeholder", usage: `{"input_tokens":0,"output_tokens":0,"total_tokens":0}`, wantNull: true},
		{name: "overflowing sum", usage: `{"input_tokens":4294967295,"output_tokens":1,"total_tokens":4294967295}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[],"usage":` + test.usage + `}`
			patched := PatchJSONBytes([]byte(raw), Options{GPTResponses: true, ContextDetailsFromUsage: true})
			var response map[string]any
			if err := json.Unmarshal(patched, &response); err != nil {
				t.Fatal(err)
			}
			if test.wantNull {
				if response["usage"] != nil {
					t.Fatalf("usage=%#v want null", response["usage"])
				}
				return
			}
			usage := response["usage"].(map[string]any)
			contextDetails, exists := usage["context_details"].(map[string]any)
			if exists != test.wantContext {
				t.Fatalf("context_details presence=%t want %t: %s", exists, test.wantContext, patched)
			}
			if test.wantContext && (contextDetails["input_tokens"] != usage["input_tokens"] ||
				contextDetails["output_tokens"] != usage["output_tokens"]) {
				t.Fatalf("context_details did not use real input/output: %s", patched)
			}
			if test.name == "complete" && usage["total_tokens"] != float64(99) {
				t.Fatalf("billing total was overwritten by live context: %s", patched)
			}
		})
	}
}

func TestContextDetailsDerivationIsOptInAndPreservesProviderValue(t *testing.T) {
	raw := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}`
	patched := PatchJSONBytes([]byte(raw), Options{GPTResponses: true})
	var response map[string]any
	if err := json.Unmarshal(patched, &response); err != nil {
		t.Fatal(err)
	}
	if _, exists := response["usage"].(map[string]any)["context_details"]; exists {
		t.Fatalf("generic Responses route received a private context extension: %s", patched)
	}

	provider := `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6,"context_details":{"input_tokens":50,"output_tokens":7,"provider_field":true}}}`
	patched = PatchJSONBytes([]byte(provider), Options{GPTResponses: true, ContextDetailsFromUsage: true})
	if err := json.Unmarshal(patched, &response); err != nil {
		t.Fatal(err)
	}
	contextDetails := response["usage"].(map[string]any)["context_details"].(map[string]any)
	if contextDetails["input_tokens"] != float64(50) || contextDetails["output_tokens"] != float64(7) || contextDetails["provider_field"] != true {
		t.Fatalf("provider context_details was changed: %s", patched)
	}
}

func TestPatchJSONBytesStrictRejectsTrailingContent(t *testing.T) {
	for _, input := range [][]byte{
		[]byte(`{"object":"response","output":[]} {"second":true}`),
		[]byte(`{"object":"response","output":[]} trailing`),
	} {
		if _, err := PatchJSONBytesStrict(input, Options{}); err == nil {
			t.Fatalf("trailing content was accepted: %q", input)
		}
	}
}

func TestPatchJSONBytesStrictRejectsNullResponse(t *testing.T) {
	if _, err := PatchJSONBytesStrict([]byte("null"), Options{}); err == nil {
		t.Fatal("null response was accepted")
	}
}

func TestPatchJSONBytesStrictRejectsNonObjectResponse(t *testing.T) {
	for _, input := range []string{`[]`, `"response"`, `42`, `true`, `false`} {
		if _, err := PatchJSONBytesStrict([]byte(input), Options{}); err == nil {
			t.Fatalf("non-object response was accepted: %s", input)
		}
	}
}

func TestPatchUsesStreamingEventStatusContext(t *testing.T) {
	created := PatchSSEDataLine(`data: {"type":"response.created","response":{"id":"resp_1","object":"response","created_at":1,"model":"m","output":[{"type":"message","id":"msg_1","role":"assistant","content":[]}]}}`, Options{})
	var createdEvent map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(created, "data:"))), &createdEvent); err != nil {
		t.Fatal(err)
	}
	response := createdEvent["response"].(map[string]any)
	item := response["output"].([]any)[0].(map[string]any)
	if response["status"] != "in_progress" || item["status"] != "in_progress" {
		t.Fatalf("created event status mismatch: %s", created)
	}

	completed := PatchSSEDataLine(`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1,"model":"m","output":[]}}`, Options{})
	var completedEvent map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(completed, "data:"))), &completedEvent); err != nil {
		t.Fatal(err)
	}
	if completedEvent["response"].(map[string]any)["status"] != "completed" {
		t.Fatalf("completed event status mismatch: %s", completed)
	}
}
