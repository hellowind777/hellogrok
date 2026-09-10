package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

func grokBuildTools() []advertisedTool {
	return []advertisedTool{
		{Name: "list_dir", Props: map[string]struct{}{"targetdirectory": {}}},
		{Name: "read_file", Props: map[string]struct{}{"targetfile": {}, "offset": {}, "limit": {}}},
		{Name: "search_replace", Props: map[string]struct{}{"filepath": {}, "oldstring": {}, "newstring": {}}},
		{Name: "run_terminal_command", Props: map[string]struct{}{"command": {}, "background": {}}},
		{Name: "grep", Props: map[string]struct{}{"pattern": {}, "path": {}}},
		{Name: "web_search", Props: map[string]struct{}{"query": {}}},
	}
}

func TestResolveAdvertisedToolNameAliasesAndShape(t *testing.T) {
	tools := grokBuildTools()
	cases := []struct {
		emitted string
		args    string
		want    string
	}{
		{emitted: "list_dir", args: `{"target_directory":"."}`, want: "list_dir"},
		{emitted: "List", args: `{"target_directory":"."}`, want: "list_dir"},
		{emitted: "LS", args: `{"path":"."}`, want: "list_dir"},
		{emitted: "Glob", args: `{"pattern":"**/*.go"}`, want: "grep"},
		{emitted: "", args: `{"target_directory":"."}`, want: "list_dir"},
		{emitted: "Read", args: `{"target_file":"a.go"}`, want: "read_file"},
		{emitted: "Bash", args: `{"command":"pwd"}`, want: "run_terminal_command"},
		{emitted: "Edit", args: `{"file_path":"a.go","old_string":"a","new_string":"b"}`, want: "search_replace"},
		{emitted: "unknown_tool", args: `{"foo":1}`, want: "unknown_tool"},
	}
	for _, test := range cases {
		got := resolveAdvertisedToolName(test.emitted, test.args, tools)
		if got != test.want {
			t.Fatalf("emitted=%q args=%s got=%q want=%q", test.emitted, test.args, got, test.want)
		}
	}
}

func TestMCPCallWrapsUseTool(t *testing.T) {
	tools := append(grokBuildTools(), advertisedTool{Name: "use_tool", Props: map[string]struct{}{"toolname": {}, "toolinput": {}}})
	name, args, notes := adaptResolvedCall("mcp__github__create_issue", `{"title":"bug"}`, tools)
	if name != "use_tool" {
		t.Fatalf("name=%q notes=%v", name, notes)
	}
	obj := parseToolArguments(args)
	if stringValue(obj["tool_name"]) != "github__create_issue" {
		t.Fatalf("wrapped args=%#v", obj)
	}
}

func TestWriteContentsMapsToWriteOrSearchReplace(t *testing.T) {
	writeTools := append(grokBuildTools(), advertisedTool{Name: "write", Props: map[string]struct{}{"filepath": {}, "content": {}}})
	name, args, _ := adaptResolvedCall("Write", `{"path":"a.go","contents":"package a"}`, writeTools)
	if name != "write" {
		t.Fatalf("write advertised: name=%q", name)
	}
	obj := parseToolArguments(args)
	if stringArg(obj, "file_path") != "a.go" || stringArg(obj, "content") != "package a" {
		t.Fatalf("write args=%#v", obj)
	}
	name, args, _ = adaptResolvedCall("Write", `{"path":"a.go","contents":"package a"}`, grokBuildTools())
	if name != "search_replace" {
		t.Fatalf("write fallback: name=%q", name)
	}
	obj = parseToolArguments(args)
	if stringArg(obj, "old_string") != "" || stringArg(obj, "new_string") != "package a" {
		t.Fatalf("search_replace write fallback args=%#v", obj)
	}
}

func TestBashMissingDescriptionIsFilled(t *testing.T) {
	_, args, notes := adaptResolvedCall("Bash", `{"command":"pwd"}`, grokBuildTools())
	obj := parseToolArguments(args)
	if stringArg(obj, "description") == "" {
		t.Fatalf("description not filled notes=%v args=%#v", notes, obj)
	}
}

func TestSpawnSubagentDefaults(t *testing.T) {
	tools := append(grokBuildTools(), advertisedTool{
		Name:  "spawn_subagent",
		Props: map[string]struct{}{"prompt": {}, "description": {}, "subagenttype": {}, "background": {}},
	})
	name, args, _ := adaptResolvedCall("Task", `{"prompt":"review the parser thoroughly"}`, tools)
	if name != "spawn_subagent" {
		t.Fatalf("name=%q", name)
	}
	obj := parseToolArguments(args)
	if stringArg(obj, "description") == "" || stringArg(obj, "subagent_type") != "general-purpose" {
		t.Fatalf("spawn args=%#v", obj)
	}
}

func TestProjectAliasesForGrokBuildToolset(t *testing.T) {
	root := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "list_dir", "description": "list", "parameters": map[string]any{"type": "object"}}},
			map[string]any{"type": "function", "function": map[string]any{"name": "read_file", "description": "read", "parameters": map[string]any{"type": "object"}}},
		},
	}
	if n := projectGrokToolAliases(root, wireChatCompletions); n == 0 {
		t.Fatal("expected alias projection")
	}
	names := map[string]bool{}
	for _, raw := range anySlice(root["tools"]) {
		names[functionToolName(raw.(map[string]any))] = true
	}
	for _, want := range []string{"list_dir", "LS", "List", "read_file", "Read"} {
		if !names[want] {
			t.Fatalf("missing %s in %v", want, names)
		}
	}
}

func TestProjectAliasesSkippedForSearchOnlyTools(t *testing.T) {
	root := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "web_fetch", "parameters": map[string]any{"type": "object"}}},
		},
	}
	if n := projectGrokToolAliases(root, wireChatCompletions); n != 0 {
		t.Fatalf("search-only tools should not grow aliases, n=%d", n)
	}
}

func TestRelayedGrokNameIsUnchanged(t *testing.T) {
	got := resolveAdvertisedToolName("list_dir", `{"target_directory":"src"}`, grokBuildTools())
	if got != "list_dir" {
		t.Fatalf("relayed grok name rewritten: %q", got)
	}
}

func TestLiftTopLevelChatToolCallName(t *testing.T) {
	call := map[string]any{
		"id":        "call_1",
		"type":      "function",
		"name":      "list_dir",
		"arguments": map[string]any{"target_directory": "."},
	}
	liftChatToolCallObject(call)
	function := call["function"].(map[string]any)
	if function["name"] != "list_dir" {
		t.Fatalf("name not lifted: %#v", function)
	}
	if _, ok := function["arguments"].(string); !ok {
		t.Fatalf("arguments not stringified: %#v", function["arguments"])
	}
}

func TestAdaptChatDeltaEmptyNameUsesShape(t *testing.T) {
	root := map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": 0,
					"id":    "call_1",
					"type":  "function",
					"function": map[string]any{
						"name":      "",
						"arguments": `{"target_directory":"."}`,
					},
				}},
			},
		}},
	}
	prepareGrokBuildToolWire(root, wireChatCompletions)
	notes := adaptGrokBuildToolIdentity(root, wireChatCompletions, grokBuildTools())
	if len(notes) == 0 {
		t.Fatal("expected identity adaptation")
	}
	delta := root["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)
	call := anySlice(delta["tool_calls"])[0].(map[string]any)
	function := call["function"].(map[string]any)
	if function["name"] != "list_dir" {
		t.Fatalf("empty name was not recovered: %#v notes=%v", function, notes)
	}
}

func TestAdaptChatLegacyFunctionCall(t *testing.T) {
	message := map[string]any{
		"role": "assistant",
		"function_call": map[string]any{
			"name":      "List",
			"arguments": `{"target_directory":"."}`,
		},
	}
	notes := adaptChatMessageTools(message, grokBuildTools(), false)
	calls := anySlice(message["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("expected converted tool_calls, got %#v notes=%v", message, notes)
	}
	function := calls[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "list_dir" {
		t.Fatalf("legacy function_call not adapted: %#v", function)
	}
}

func TestRewriteListDirPathArgument(t *testing.T) {
	rewritten, changed := rewriteAdvertisedArguments(`{"path":"."}`, grokBuildTools()[0])
	if !changed || !strings.Contains(rewritten, `"target_directory"`) {
		t.Fatalf("path was not rewritten: changed=%t body=%s", changed, rewritten)
	}
}

func TestUnwrapNestedInputArguments(t *testing.T) {
	name := resolveAdvertisedToolName("", `{"input":{"target_directory":"."}}`, grokBuildTools())
	if name != "list_dir" {
		t.Fatalf("nested input was not unwrapped: %q", name)
	}
}

func TestBackfillChatToolResultName(t *testing.T) {
	messages := []any{
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{map[string]any{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "list_dir", "arguments": `{"target_directory":"."}`},
			}},
		},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "ok"},
	}
	root := map[string]any{"messages": messages}
	notes := adaptGrokBuildToolIdentity(root, wireChatCompletions, grokBuildTools())
	if stringValue(messages[1].(map[string]any)["name"]) != "list_dir" {
		t.Fatalf("tool result name not backfilled: %#v notes=%v", messages[1], notes)
	}
}

func TestCollectAdvertisedFunctionToolsFromChat(t *testing.T) {
	root := map[string]any{
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name": "list_dir",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"target_directory": map[string]any{"type": "string"},
					},
				},
			}},
			map[string]any{"type": "web_search"},
		},
	}
	tools := collectAdvertisedFunctionTools(root, wireChatCompletions)
	if len(tools) != 1 || tools[0].Name != "list_dir" {
		t.Fatalf("unexpected advertised tools: %#v", tools)
	}
	if _, ok := tools[0].Props["targetdirectory"]; !ok {
		t.Fatalf("missing schema property: %#v", tools[0].Props)
	}
}

func TestExtractKimiXMLToolCall(t *testing.T) {
	content := "thinking\n<tool_call>\n<function=list_dir>\n<parameter=target_directory>.</parameter>\n</function>\n</tool_call>"
	calls, rest, ok := extractToolCallsFromText(content)
	if !ok || len(calls) != 1 || rest != "thinking" {
		t.Fatalf("xml extract failed ok=%t rest=%q calls=%#v", ok, rest, calls)
	}
	function := calls[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "list_dir" {
		t.Fatalf("xml name: %#v", function)
	}
	if !strings.Contains(stringValue(function["arguments"]), "target_directory") {
		t.Fatalf("xml args: %#v", function["arguments"])
	}
}

func TestExtractJSONToolCallFromContent(t *testing.T) {
	content := `<tool_call>{"name":"Read","arguments":{"target_file":"a.go"}}</tool_call>`
	message := map[string]any{"role": "assistant", "content": content}
	notes := adaptChatMessageTools(message, grokBuildTools(), true)
	calls := anySlice(message["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("json content extract failed notes=%v message=%#v", notes, message)
	}
	function := calls[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "read_file" {
		t.Fatalf("json content name: %#v", function)
	}
}

func TestAdaptRequestHistoryUnknownListName(t *testing.T) {
	root := map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "list files"},
			map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"name":     "List",
					"function": map[string]any{"arguments": `{"target_directory":"."}`},
				}},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "failed"},
		},
	}
	prepareGrokBuildToolWire(root, wireChatCompletions)
	notes := adaptGrokBuildToolIdentity(root, wireChatCompletions, grokBuildTools())
	call := anySlice(root["messages"].([]any)[1].(map[string]any)["tool_calls"])[0].(map[string]any)
	function := call["function"].(map[string]any)
	if function["name"] != "list_dir" {
		t.Fatalf("history name not repaired: %#v notes=%v", function, notes)
	}
	result := root["messages"].([]any)[2].(map[string]any)
	if stringValue(result["name"]) != "list_dir" {
		t.Fatalf("history tool result name: %#v", result)
	}
}

func TestDoNotInventUnadvertisedTool(t *testing.T) {
	tools := []advertisedTool{{Name: "read_file", Props: map[string]struct{}{"targetfile": {}}}}
	if got := resolveAdvertisedToolName("List", `{"target_directory":"."}`, tools); got != "List" {
		t.Fatalf("invented unadvertised tool: %q", got)
	}
}

func TestAdaptFacadeRequestRepairsChatHistoryNames(t *testing.T) {
	body := []byte(`{
		"model":"Kimi-K3-ipix","stream":true,
		"messages":[
			{"role":"user","content":"list files"},
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","name":"List","function":{"arguments":"{\"target_directory\":\".\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"failed"}
		],
		"tools":[{"type":"function","function":{"name":"list_dir","parameters":{"type":"object","properties":{"target_directory":{"type":"string"}}}}}]
	}`)
	route := config.Route{
		ChannelID: "Kimi-K3-ipix", APIBackend: "chat_completions", APIBackendConfigured: true,
		OriginBase: "https://api.example.com/v1", WireModel: "Kimi-K3",
	}
	request, err := adaptFacadeRequest(body, route, wireChatCompletions)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.AdvertisedTools) != 1 || request.AdvertisedTools[0].Name != "list_dir" {
		t.Fatalf("advertised=%#v", request.AdvertisedTools)
	}
	root, err := decodeJSONMap(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	assistant := anySlice(root["messages"])[1].(map[string]any)
	call := anySlice(assistant["tool_calls"])[0].(map[string]any)
	if stringValue(call["function"].(map[string]any)["name"]) != "list_dir" {
		t.Fatalf("request history was not repaired: %s", request.Body)
	}
	result := anySlice(root["messages"])[2].(map[string]any)
	if stringValue(result["name"]) != "list_dir" {
		t.Fatalf("tool result name missing: %#v", result)
	}
}

func TestCanonicalFromChatLiftsAndResolves(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"id": "chatcmpl_1", "object": "chat.completion", "created": 1, "model": "kimi",
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "tool_calls",
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id": "call_1", "type": "function",
					"name":      "List",
					"arguments": `{"target_directory":"."}`,
				}},
			},
		}},
	})
	result, err := canonicalFromChat(body, false, "", grokBuildTools())
	if err != nil {
		t.Fatal(err)
	}
	var found string
	for _, raw := range result.Output {
		item, _ := raw.(map[string]any)
		if stringValue(item["type"]) == "function_call" {
			found = stringValue(item["name"])
		}
	}
	if found != "list_dir" {
		t.Fatalf("canonical chat name=%q output=%#v", found, result.Output)
	}
}
