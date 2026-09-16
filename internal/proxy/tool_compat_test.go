package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

func grokBuildTools() []advertisedTool {
	return []advertisedTool{
		{Name: "list_dir", HasRequired: true, Props: map[string]struct{}{"targetdirectory": {}}},
		{Name: "read_file", HasRequired: true, Props: map[string]struct{}{"targetfile": {}, "offset": {}, "limit": {}}},
		{Name: "search_replace", HasRequired: true, Props: map[string]struct{}{"filepath": {}, "oldstring": {}, "newstring": {}}},
		{Name: "run_terminal_command", HasRequired: true, Props: map[string]struct{}{"command": {}, "background": {}}},
		{Name: "grep", HasRequired: true, Props: map[string]struct{}{"pattern": {}, "path": {}}},
		{Name: "web_search", HasRequired: true, Props: map[string]struct{}{"query": {}}},
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

func TestAdaptResolvedCallLeavesIncompleteJSON(t *testing.T) {
	name, args, notes := adaptResolvedCall("run_terminal_command", `{"command":"git`, grokBuildTools())
	if name != "run_terminal_command" {
		t.Fatalf("name=%q", name)
	}
	if args != `{"command":"git` {
		t.Fatalf("incomplete JSON rewritten args=%q notes=%v", args, notes)
	}
	for _, note := range notes {
		if strings.Contains(note, "-canon") {
			t.Fatalf("canon ran on incomplete JSON notes=%v", notes)
		}
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

func TestNativeChatKeepsGrokClientToolNamesAndIncludeUsage(t *testing.T) {
	route := config.Route{
		ChannelID:            "Kimi-K3-ipix",
		WireModel:            "Kimi-K3",
		APIBackend:           "chat_completions",
		APIBackendConfigured: true,
	}
	body := []byte(`{"model":"Kimi-K3-ipix","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object","properties":{}}}},{"type":"function","function":{"name":"list_dir","parameters":{"type":"object","properties":{}}}}]}`)
	request, err := adaptFacadeRequest(body, route, wireChatCompletions)
	if err != nil {
		t.Fatal(err)
	}
	root, err := decodeRequestObject(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, raw := range anySlice(root["tools"]) {
		tool, _ := raw.(map[string]any)
		names[functionToolName(tool)] = true
	}
	if !names["read_file"] || !names["list_dir"] {
		t.Fatalf("grok client names missing: %v", names)
	}
	for _, extra := range []string{"Read", "LS", "List", "Grep", "Bash", "Edit"} {
		if names[extra] {
			t.Fatalf("claude alias projected onto native wire: %s in %v", extra, names)
		}
	}
	options, _ := root["stream_options"].(map[string]any)
	if options["include_usage"] != true {
		t.Fatalf("include_usage missing: %#v", root["stream_options"])
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

func TestRewriteTargetPathArgument(t *testing.T) {
	byName := advertisedByName(grokBuildTools())
	cases := []struct {
		tool string
		args string
		want string
	}{
		{tool: "read_file", args: `{"target_path":"a.go"}`, want: `"target_file":"a.go"`},
		{tool: "list_dir", args: `{"target_path":"."}`, want: `"target_directory":"."`},
	}
	for _, test := range cases {
		rewritten, changed := rewriteAdvertisedArguments(test.args, byName[test.tool])
		if !changed || !strings.Contains(rewritten, test.want) || strings.Contains(rewritten, `"target_path"`) {
			t.Fatalf("%s: target_path not rewritten: changed=%t body=%s", test.tool, changed, rewritten)
		}
	}
	writeTool := advertisedTool{Name: "write", Props: map[string]struct{}{"filepath": {}, "content": {}}}
	rewritten, changed := rewriteAdvertisedArguments(`{"target_path":"a.go","content":"x"}`, writeTool)
	if !changed || !strings.Contains(rewritten, `"file_path":"a.go"`) {
		t.Fatalf("write: target_path not rewritten: changed=%t body=%s", changed, rewritten)
	}
	// Tools without an advertised path property must keep the argument untouched.
	if rewritten, changed := rewriteAdvertisedArguments(`{"target_path":"a.go"}`, byName["grep"]); changed {
		t.Fatalf("grep args must stay untouched: body=%s", rewritten)
	}
	name, args, notes := adaptResolvedCall("read_file", `{"target_path":"a.go"}`, grokBuildTools())
	if name != "read_file" || !strings.Contains(args, `"target_file":"a.go"`) {
		t.Fatalf("adaptResolvedCall: name=%q args=%s notes=%v", name, args, notes)
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

func TestExtractInvokeAndFencedToolCalls(t *testing.T) {
	invoke := `note
<invoke name="list_dir">
<parameter name="target_directory">.</parameter>
</invoke>`
	calls, rest, ok := extractToolCallsFromText(invoke)
	if !ok || len(calls) != 1 || rest != "note" {
		t.Fatalf("invoke extract failed ok=%t rest=%q calls=%#v", ok, rest, calls)
	}
	function := calls[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "list_dir" {
		t.Fatalf("invoke name: %#v", function)
	}
	if !strings.Contains(stringValue(function["arguments"]), "target_directory") {
		t.Fatalf("invoke args: %#v", function["arguments"])
	}

	fenced := "pre\n```tool_call\n{\"name\":\"Read\",\"arguments\":{\"target_file\":\"a.go\"}}\n```"
	message := map[string]any{"role": "assistant", "content": fenced}
	notes := adaptChatMessageTools(message, grokBuildTools(), true)
	got := anySlice(message["tool_calls"])
	if len(got) != 1 {
		t.Fatalf("fenced extract failed notes=%v message=%#v", notes, message)
	}
	function = got[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "read_file" {
		t.Fatalf("fenced name: %#v", function)
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

// shapeTieTools mirrors the production ambiguity: run_terminal_command and
// monitor advertise the same command+description properties, so a name-less
// call with exactly that key set never matches uniquely by shape alone.
func shapeTieTools() []advertisedTool {
	return []advertisedTool{
		{Name: "run_terminal_command", Props: map[string]struct{}{"command": {}, "description": {}, "timeout": {}, "background": {}}},
		{Name: "monitor", Props: map[string]struct{}{"command": {}, "description": {}, "persistent": {}, "timeoutms": {}}},
		{Name: "read_file", Props: map[string]struct{}{"targetfile": {}, "offset": {}, "limit": {}}},
	}
}

func TestRepairToolArgumentsPrefix(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{in: `command":"pwd","description":"list"}`, want: `{"command":"pwd","description":"list"}`, ok: true},
		{in: `limit":30,"offset":1660,"target_file":"a.py"}`, want: `{"limit":30,"offset":1660,"target_file":"a.py"}`, ok: true},
		{in: `command":"git stat`, want: `command":"git stat`, ok: false},
		{in: `{"command":"pw`, want: `{"command":"pw`, ok: false},
		{in: `{"command":"pwd"}`, want: `{"command":"pwd"}`, ok: false},
		{in: ``, want: ``, ok: false},
	}
	for _, tc := range cases {
		got, ok := repairToolArgumentsPrefix(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("repair(%q) = (%q, %t), want (%q, %t)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestResolveAdvertisedToolNameTieBreaksToTerminal(t *testing.T) {
	got := resolveAdvertisedToolName("", `{"command":"pwd","description":"list"}`, shapeTieTools())
	if got != "run_terminal_command" {
		t.Fatalf("tie not broken: %q", got)
	}
	// A monitor-only key set still resolves to monitor by unique shape.
	got = resolveAdvertisedToolName("", `{"command":"npm run dev","description":"dev server","persistent":true}`, shapeTieTools())
	if got != "monitor" {
		t.Fatalf("monitor shape lost: %q", got)
	}
}

func TestAdaptResolvedCallRepairsTruncatedArguments(t *testing.T) {
	name, args, notes := adaptResolvedCall("", `command":"pwd","description":"list"}`, shapeTieTools())
	if name != "run_terminal_command" {
		t.Fatalf("name=%q notes=%v", name, notes)
	}
	if !jsonObjectComplete(args) {
		t.Fatalf("arguments not repaired: %q", args)
	}
	obj := parseToolArguments(args)
	if stringArg(obj, "command") != "pwd" {
		t.Fatalf("command lost: %#v", obj)
	}
	if !strings.Contains(strings.Join(notes, ","), "args-prefix-repaired") {
		t.Fatalf("repair not noted: %v", notes)
	}
}

// Broken calls persisted in an earlier turn (empty name, arguments truncated
// by the lost first delta) are replayed in every later request; the request
// side must repair them before they reach the upstream, and propagate the
// repaired name onto the matching tool result message.
func TestAdaptRequestHistoryRepairsBrokenAssistantCall(t *testing.T) {
	root := map[string]any{
		"model": "GLM-5.3",
		"messages": []any{
			map[string]any{"role": "user", "content": "check git"},
			map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id": "call_broken", "type": "function",
					"function": map[string]any{
						"name":      "",
						"arguments": `command":"git status","description":"check git"}`,
					},
				}},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_broken", "content": "failed"},
		},
	}
	adaptGrokBuildToolIdentity(root, wireChatCompletions, shapeTieTools())
	messages := anySlice(root["messages"])
	assistant, _ := messages[1].(map[string]any)
	call, _ := anySlice(assistant["tool_calls"])[0].(map[string]any)
	function, _ := call["function"].(map[string]any)
	if stringValue(function["name"]) != "run_terminal_command" {
		t.Fatalf("history name not repaired: %#v", function)
	}
	if !jsonObjectComplete(stringValue(function["arguments"])) {
		t.Fatalf("history arguments not repaired: %#v", function)
	}
	result, _ := messages[2].(map[string]any)
	if stringValue(result["name"]) != "run_terminal_command" {
		t.Fatalf("tool result name not backfilled: %#v", result)
	}
}
