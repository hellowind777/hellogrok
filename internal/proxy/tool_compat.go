package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// advertisedTool is one Grok Build function tool as declared on the current
// request. Response-side identity repair may only resolve to these names.
type advertisedTool struct {
	Name  string
	Props map[string]struct{}
}

var grokToolAliasIndex = buildGrokToolAliasIndex(map[string][]string{
	"list_dir":                       {"list", "ls", "listdir", "listdirectory", "listfiles", "lsdir"},
	"glob":                           {"glob", "globfile"},
	"read_file":                      {"read", "readfile", "hashlineread"},
	"search_replace":                 {"edit", "multiedit", "notebookedit", "strreplace", "strreplacebasededittool", "applypatch", "apply_patch"},
	"run_terminal_command":           {"bash", "shell", "powershell", "terminal", "execute", "runterminalcmd", "runcommand", "command"},
	"grep":                           {"search", "searchcodebase", "hashlinegrep", "rg"},
	"web_search":                     {"websearch", "searchweb"},
	"web_fetch":                      {"webfetch", "fetch", "openurl"},
	"todo_write":                     {"todowrite", "todo"},
	"spawn_subagent":                 {"task", "agent", "spawn"},
	"get_command_or_subagent_output": {"bashoutput", "taskoutput", "bashoutputtool", "agentoutputtool", "taskoutputtool"},
	"kill_command_or_subagent":       {"killbash", "killshell", "taskstop", "killtask"},
	"wait_commands_or_subagents":     {"wait", "waittasks", "await"},
	"search_tool":                    {"toolsearch", "searchtool"},
	"use_tool":                       {"usetool", "callmcp"},
	"ask_user_question":              {"askuserquestion", "askuser"},
	"enter_plan_mode":                {"enterplanmode"},
	"exit_plan_mode":                 {"exitplanmode"},
	"scheduler_create":               {"croncreate", "schedulecreate"},
	"scheduler_delete":               {"crondelete", "scheduledelete"},
	"scheduler_list":                 {"cronlist", "schedulelist"},
	"skill":                          {"skill"},
	"monitor":                        {"monitor"},
	"workflow":                       {"workflow"},
	"send_feedback":                  {"feedback", "sendfeedback"},
	"update_goal":                    {"updategoal"},
	"image_gen":                      {"imagegen", "generateimage"},
	"image_edit":                     {"imageedit", "editimage"},
	"image_to_video":                 {"imagetovideo"},
	"reference_to_video":             {"referencetovideo"},
	"lsp":                            {"lsp"},
	"write":                          {"writefile", "createfile"},
})

var grokParamAliasIndex = map[string][]string{
	"target_directory": {"path", "directory", "dir", "targetdir", "target_dir", "folder"},
	"target_file":      {"path", "filepath", "file_path", "file", "filename"},
	"file_path":        {"path", "filepath", "targetfile", "target_file", "file"},
	"command":          {"cmd", "script", "code", "shell"},
	"pattern":          {"query", "regex", "search", "globpattern", "glob_pattern"},
	"old_string":       {"oldstring", "from"},
	"new_string":       {"newstring", "replace", "to"},
	"url":              {"uri", "href", "link"},
	"query":            {"q", "search", "prompt", "text"},
	"content":          {"contents", "body", "text", "filecontent"},
	"background":       {"run_in_background", "is_background", "async"},
	"skill":            {"name", "skillname", "skill_name"},
	"glob":             {"globpattern", "glob_pattern"},
	"description":      {"desc", "title", "summary"},
	"prompt":           {"task", "instruction", "message"},
	"subagent_type":    {"agent", "agent_type", "type"},
}

func buildGrokToolAliasIndex(table map[string][]string) map[string][]string {
	index := map[string][]string{}
	add := func(key, target string) {
		key = compactToolName(key)
		if key == "" {
			return
		}
		for _, existing := range index[key] {
			if existing == target {
				return
			}
		}
		index[key] = append(index[key], target)
	}
	for target, aliases := range table {
		add(target, target)
		for _, alias := range aliases {
			add(alias, target)
		}
	}
	return index
}

func compactToolName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func collectAdvertisedFunctionTools(root map[string]any, protocol wireProtocol) []advertisedTool {
	var tools []advertisedTool
	seen := map[string]struct{}{}
	for _, raw := range anySlice(root["tools"]) {
		tool, _ := raw.(map[string]any)
		name := strings.TrimSpace(functionToolName(tool))
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		tools = append(tools, advertisedTool{
			Name:  name,
			Props: schemaPropertyNames(toolSchema(tool, protocol)),
		})
	}
	return tools
}

func toolSchema(tool map[string]any, protocol wireProtocol) any {
	if tool == nil {
		return nil
	}
	if protocol == wireMessages {
		if schema := tool["input_schema"]; schema != nil {
			return schema
		}
	}
	if fn, _ := tool["function"].(map[string]any); fn != nil {
		if schema := fn["parameters"]; schema != nil {
			return schema
		}
	}
	return tool["parameters"]
}

func schemaPropertyNames(schema any) map[string]struct{} {
	obj, _ := schema.(map[string]any)
	if obj == nil {
		return nil
	}
	props, _ := obj["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	names := make(map[string]struct{}, len(props))
	for name := range props {
		if compact := compactToolName(name); compact != "" {
			names[compact] = struct{}{}
		}
	}
	return names
}

func advertisedByName(tools []advertisedTool) map[string]advertisedTool {
	index := make(map[string]advertisedTool, len(tools))
	for _, tool := range tools {
		index[strings.ToLower(tool.Name)] = tool
		if compact := compactToolName(tool.Name); compact != "" {
			if _, exists := index[compact]; !exists {
				index[compact] = tool
			}
		}
	}
	return index
}

func advertisedHas(tools []advertisedTool, name string) bool {
	_, ok := advertisedToolNamed(tools, name)
	return ok
}

func advertisedToolNamed(tools []advertisedTool, name string) (advertisedTool, bool) {
	index := advertisedByName(tools)
	if tool, ok := index[strings.ToLower(name)]; ok {
		return tool, true
	}
	tool, ok := index[compactToolName(name)]
	return tool, ok
}

func advertisedCanonicalName(tools []advertisedTool, name string) string {
	if tool, ok := advertisedToolNamed(tools, name); ok {
		return tool.Name
	}
	return name
}

func resolveAdvertisedToolName(emitted, arguments string, advertised []advertisedTool) string {
	if len(advertised) == 0 {
		return strings.TrimSpace(emitted)
	}
	args := parseToolArguments(arguments)
	if looksLikeFileGlob(emitted, args) {
		for _, name := range []string{"glob", "grep", "run_terminal_command"} {
			if advertisedHas(advertised, name) {
				return advertisedCanonicalName(advertised, name)
			}
		}
	}
	if looksLikeWrite(args) {
		for _, name := range []string{"write", "search_replace"} {
			if advertisedHas(advertised, name) {
				return advertisedCanonicalName(advertised, name)
			}
		}
	}
	index := advertisedByName(advertised)
	if name := strings.TrimSpace(emitted); name != "" {
		if tool, ok := index[strings.ToLower(name)]; ok {
			return tool.Name
		}
		if tool, ok := index[compactToolName(name)]; ok {
			return tool.Name
		}
		for _, target := range grokToolAliasIndex[compactToolName(name)] {
			if tool, ok := index[strings.ToLower(target)]; ok {
				return tool.Name
			}
		}
		if mcp := normalizeMCPToolName(name); mcp != "" && advertisedHas(advertised, "use_tool") {
			return advertisedCanonicalName(advertised, "use_tool")
		}
	}
	if tool := uniqueShapeMatch(args, advertised); tool != nil {
		return tool.Name
	}
	return strings.TrimSpace(emitted)
}

func uniqueShapeMatch(args map[string]any, advertised []advertisedTool) *advertisedTool {
	args = unwrapToolArguments(args)
	if len(args) == 0 {
		return nil
	}
	argKeys := map[string]struct{}{}
	for key := range args {
		if compact := compactToolName(key); compact != "" {
			argKeys[compact] = struct{}{}
		}
	}
	var matches []advertisedTool
	best := 0
	for _, tool := range advertised {
		score := 0
		for key := range argKeys {
			if _, ok := tool.Props[key]; ok {
				score++
			}
		}
		if score == 0 {
			continue
		}
		if score > best {
			best = score
			matches = matches[:0]
			matches = append(matches, tool)
			continue
		}
		if score == best {
			matches = append(matches, tool)
		}
	}
	if len(matches) != 1 || best == 0 {
		return shapeTieBreak(matches)
	}
	match := matches[0]
	return &match
}

// shapeTieBreakPriority resolves shape matches that stay ambiguous because
// several advertised tools share every emitted property: run_terminal_command
// and monitor both accept exactly command+description, so a name-less call
// with that key set never matches uniquely. One-shot shell commands are the
// dominant real-world case, so they win the tie; anything else stays
// unrepaired instead of being guessed.
var shapeTieBreakPriority = []string{"run_terminal_command"}

func shapeTieBreak(matches []advertisedTool) *advertisedTool {
	for _, want := range shapeTieBreakPriority {
		compact := compactToolName(want)
		for i := range matches {
			if compactToolName(matches[i].Name) == compact {
				return &matches[i]
			}
		}
	}
	return nil
}

func parseToolArguments(value any) map[string]any {
	switch typed := value.(type) {
	case map[string]any:
		return typed
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return nil
		}
		var obj map[string]any
		if json.Unmarshal([]byte(trimmed), &obj) != nil {
			return nil
		}
		return obj
	default:
		if value == nil {
			return nil
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		var obj map[string]any
		if json.Unmarshal(encoded, &obj) != nil {
			return nil
		}
		return obj
	}
}

func unwrapToolArguments(obj map[string]any) map[string]any {
	if obj == nil || len(obj) != 1 {
		return obj
	}
	for key, child := range obj {
		switch compactToolName(key) {
		case "input", "arguments", "parameters", "args", "params":
			if nested := parseToolArguments(child); len(nested) > 0 {
				return nested
			}
		}
	}
	return obj
}

func encodeToolArguments(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case nil:
		return ""
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

func rewriteAdvertisedArguments(arguments string, tool advertisedTool) (string, bool) {
	obj := parseToolArguments(arguments)
	if len(obj) == 0 {
		return arguments, false
	}
	unwrapped := unwrapToolArguments(obj)
	changed := len(unwrapped) != len(obj)
	obj = unwrapped
	for canonical, aliases := range grokParamAliasIndex {
		if _, exists := obj[canonical]; exists {
			continue
		}
		if _, advertised := tool.Props[compactToolName(canonical)]; !advertised {
			continue
		}
		for _, alias := range aliases {
			if value, ok := obj[alias]; ok {
				obj[canonical] = value
				delete(obj, alias)
				changed = true
				break
			}
		}
	}
	if !changed {
		return arguments, false
	}
	encoded, err := json.Marshal(obj)
	if err != nil {
		return arguments, false
	}
	return string(encoded), true
}

func firstArg(args map[string]any, keys ...string) any {
	for _, key := range keys {
		value, ok := args[key]
		if !ok || value == nil {
			continue
		}
		if text, ok := value.(string); ok && strings.TrimSpace(text) == "" {
			continue
		}
		return value
	}
	return nil
}

func stringArg(args map[string]any, key string) string {
	text, _ := args[key].(string)
	return strings.TrimSpace(text)
}

func looksLikeWrite(args map[string]any) bool {
	args = unwrapToolArguments(args)
	if len(args) == 0 {
		return false
	}
	if firstArg(args, "old_string") != nil {
		return false
	}
	return firstArg(args, "content", "contents", "body") != nil &&
		firstArg(args, "file_path", "path", "target_file") != nil
}

func looksLikeFileGlob(emitted string, args map[string]any) bool {
	switch compactToolName(emitted) {
	case "glob", "globfile":
		return true
	}
	args = unwrapToolArguments(args)
	if len(args) == 0 {
		return false
	}
	if firstArg(args, "glob_pattern", "globpattern") != nil {
		return true
	}
	if firstArg(args, "old_string", "command", "query", "target_directory") != nil {
		return false
	}
	pattern := stringArg(args, "pattern")
	return looksLikeGlobPattern(pattern)
}

func looksLikeGlobPattern(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

func normalizeMCPToolName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	name = strings.TrimPrefix(name, "mcp__")
	if strings.Count(name, "__") != 1 {
		return ""
	}
	server, tool, ok := strings.Cut(name, "__")
	if !ok || strings.TrimSpace(server) == "" || strings.TrimSpace(tool) == "" {
		return ""
	}
	return name
}

func wrapMCPUseToolArgs(mcpName, arguments string) string {
	input := parseToolArguments(arguments)
	if input == nil {
		input = map[string]any{}
	}
	return encodeToolArguments(map[string]any{
		"tool_name":  mcpName,
		"tool_input": input,
	})
}

func truncateWords(text string, n int) string {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) == 0 {
		return "task"
	}
	if len(fields) > n {
		fields = fields[:n]
	}
	return strings.Join(fields, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "'.'"
	}
	if !strings.ContainsAny(value, " \t\n'\"\\$`") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func jsonObjectComplete(arguments string) bool {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return false
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return false
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return false
	}
	return true
}

// repairToolArgumentsPrefix restores the leading `{"` (or `{`) that a relay
// dropped from the first streamed arguments fragment. Relays that omit
// function.name on tool-call deltas sometimes truncate the opening of the
// arguments object as well; without the prefix the accumulated arguments are
// not parseable JSON, so name inference and argument canonicalization cannot
// run and Grok Build reports the call as NotFound.
func repairToolArgumentsPrefix(args string) (string, bool) {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" || strings.HasPrefix(trimmed, "{") || jsonObjectComplete(trimmed) {
		return args, false
	}
	for _, prefix := range []string{`{"`, `{`} {
		if candidate := prefix + trimmed; jsonObjectComplete(candidate) {
			return candidate, true
		}
	}
	return args, false
}

func canonicalizeToolArguments(toolName, arguments string, advertised []advertisedTool) (string, bool) {
	if strings.TrimSpace(arguments) != "" && !jsonObjectComplete(arguments) {
		return arguments, false
	}
	obj := parseToolArguments(arguments)
	if obj == nil {
		obj = map[string]any{}
	}
	unwrapped := unwrapToolArguments(obj)
	changed := len(unwrapped) != len(obj)
	obj = unwrapped
	switch compactToolName(toolName) {
	case "write":
		if _, ok := obj["content"]; !ok {
			if value := firstArg(obj, "contents", "body", "text"); value != nil {
				obj["content"] = value
				changed = true
			}
		}
	case "searchreplace":
		if looksLikeWrite(obj) {
			content := firstArg(obj, "content", "contents")
			if firstArg(obj, "old_string") == nil && content != nil {
				obj["old_string"] = ""
				obj["new_string"] = content
				delete(obj, "content")
				delete(obj, "contents")
				changed = true
			}
		}
	case "grep":
		if _, ok := obj["glob"]; !ok {
			if value := firstArg(obj, "glob_pattern", "globpattern"); value != nil {
				obj["glob"] = value
				changed = true
			}
		}
		pattern := stringArg(obj, "pattern")
		if looksLikeGlobPattern(pattern) && firstArg(obj, "glob") == nil {
			obj["glob"] = pattern
			obj["pattern"] = "."
			changed = true
		} else if stringArg(obj, "pattern") == "" && firstArg(obj, "glob") != nil {
			obj["pattern"] = "."
			changed = true
		}
	case "glob":
		if _, ok := obj["pattern"]; !ok {
			if value := firstArg(obj, "glob_pattern", "globpattern"); value != nil {
				obj["pattern"] = value
				changed = true
			}
		}
	case "runterminalcommand":
		if stringArg(obj, "command") == "" {
			if glob := firstArg(obj, "pattern", "glob", "glob_pattern", "globpattern"); glob != nil {
				path := "."
				if value := firstArg(obj, "path", "target_directory"); value != nil {
					path = fmt.Sprint(value)
				}
				obj["command"] = fmt.Sprintf("rg --files --glob %s -- %s", shellQuote(fmt.Sprint(glob)), shellQuote(path))
				changed = true
			}
		}
		if stringArg(obj, "description") == "" {
			command := stringArg(obj, "command")
			if command == "" {
				obj["description"] = "Run command"
			} else {
				obj["description"] = truncateWords(command, 8)
			}
			changed = true
		}
	case "spawnsubagent":
		if stringArg(obj, "description") == "" {
			prompt := stringArg(obj, "prompt")
			if prompt == "" {
				prompt = "delegated task"
			}
			obj["description"] = truncateWords(prompt, 5)
			changed = true
		}
		if stringArg(obj, "subagent_type") == "" {
			obj["subagent_type"] = "general-purpose"
			changed = true
		}
	case "skill":
		if stringArg(obj, "skill") == "" {
			if value := firstArg(obj, "name", "skill_name", "skillname"); value != nil {
				obj["skill"] = value
				changed = true
			}
		}
	}
	_ = advertised
	if !changed {
		return arguments, false
	}
	return encodeToolArguments(obj), true
}

func adaptResolvedCall(emitted, arguments string, advertised []advertisedTool) (string, string, []string) {
	emitted = strings.TrimSpace(emitted)
	args := arguments
	var notes []string
	if repaired, ok := repairToolArgumentsPrefix(args); ok {
		args = repaired
		notes = append(notes, "args-prefix-repaired")
	}
	name := resolveAdvertisedToolName(emitted, args, advertised)
	if mcp := normalizeMCPToolName(emitted); mcp != "" && compactToolName(name) == "usetool" {
		args = wrapMCPUseToolArgs(mcp, args)
		notes = append(notes, emitted+"->use_tool")
	} else if name != "" && name != emitted {
		notes = append(notes, emitted+"->"+name)
	}
	if strings.TrimSpace(args) != "" && !jsonObjectComplete(args) {
		return name, args, notes
	}
	if tool, ok := advertisedToolNamed(advertised, name); ok {
		if rewritten, changed := rewriteAdvertisedArguments(args, tool); changed {
			args = rewritten
			notes = append(notes, tool.Name+"-args")
		}
		if rewritten, changed := canonicalizeToolArguments(name, args, advertised); changed {
			args = rewritten
			notes = append(notes, tool.Name+"-canon")
		}
	}
	return name, args, notes
}

func cloneJSONMap(value map[string]any) map[string]any {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(data, &out) != nil {
		return nil
	}
	return out
}

func prepareGrokBuildToolWire(root map[string]any, protocol wireProtocol) {
	switch protocol {
	case wireChatCompletions:
		for _, raw := range anySlice(root["choices"]) {
			choice, _ := raw.(map[string]any)
			for _, key := range []string{"message", "delta"} {
				liftChatMessageToolCalls(choice[key])
			}
		}
		for _, raw := range anySlice(root["messages"]) {
			message, _ := raw.(map[string]any)
			liftChatMessageToolCalls(message)
		}
	}
}

func adaptGrokBuildToolIdentity(root map[string]any, protocol wireProtocol, advertised []advertisedTool) []string {
	if root == nil {
		return nil
	}
	var notes []string
	switch protocol {
	case wireChatCompletions:
		for _, raw := range anySlice(root["choices"]) {
			choice, _ := raw.(map[string]any)
			for _, key := range []string{"message", "delta"} {
				message, _ := choice[key].(map[string]any)
				notes = append(notes, adaptChatMessageTools(message, advertised, key == "message")...)
			}
		}
		for _, raw := range anySlice(root["messages"]) {
			message, _ := raw.(map[string]any)
			notes = append(notes, adaptChatMessageTools(message, advertised, false)...)
		}
		notes = append(notes, backfillChatToolResultNames(anySlice(root["messages"]))...)
	case wireResponses:
		notes = append(notes, adaptResponsesItems(root["input"], advertised)...)
		notes = append(notes, adaptResponsesItems(root["output"], advertised)...)
		if item, _ := root["item"].(map[string]any); item != nil {
			notes = append(notes, adaptResponsesItem(item, advertised)...)
		}
		if response, _ := root["response"].(map[string]any); response != nil {
			notes = append(notes, adaptResponsesItems(response["output"], advertised)...)
		}
	case wireMessages:
		for _, raw := range anySlice(root["messages"]) {
			message, _ := raw.(map[string]any)
			notes = append(notes, adaptMessagesContent(message["content"], advertised)...)
		}
		notes = append(notes, adaptMessagesContent(root["content"], advertised)...)
		if message, _ := root["message"].(map[string]any); message != nil {
			notes = append(notes, adaptMessagesContent(message["content"], advertised)...)
		}
		if block, _ := root["content_block"].(map[string]any); block != nil {
			notes = append(notes, adaptMessagesToolUse(block, advertised)...)
		}
		if delta, _ := root["delta"].(map[string]any); delta != nil {
			if name := strings.TrimSpace(stringValue(delta["name"])); name != "" {
				if resolved := resolveAdvertisedToolName(name, "", advertised); resolved != "" && resolved != name {
					delta["name"] = resolved
					notes = append(notes, name+"->"+resolved)
				}
			}
		}
	}
	return uniqueStrings(notes)
}

func liftChatMessageToolCalls(value any) {
	message, _ := value.(map[string]any)
	if message == nil {
		return
	}
	wrapToolCallsArray(message)
	if len(anySlice(message["tool_calls"])) == 0 {
		if call, _ := message["function_call"].(map[string]any); call != nil {
			wrapped := cloneMap(call)
			if stringValue(wrapped["type"]) == "" {
				wrapped["type"] = "function"
			}
			liftChatToolCallObject(wrapped)
			if stringValue(wrapped["id"]) == "" {
				wrapped["id"] = compatID("call")
			}
			message["tool_calls"] = []any{wrapped}
		}
	}
	for _, raw := range anySlice(message["tool_calls"]) {
		call, _ := raw.(map[string]any)
		liftChatToolCallObject(call)
	}
}

func liftChatToolCallObject(call map[string]any) {
	if call == nil {
		return
	}
	if stringValue(call["type"]) == "" {
		call["type"] = "function"
	}
	function, _ := call["function"].(map[string]any)
	if function == nil {
		function = map[string]any{}
		call["function"] = function
	}
	if stringValue(function["name"]) == "" {
		if name := firstString(call, "name"); name != "" {
			function["name"] = name
		}
	}
	if function["arguments"] == nil {
		if args, ok := call["arguments"]; ok {
			function["arguments"] = args
		} else if args, ok := call["args"]; ok {
			function["arguments"] = args
		} else if args, ok := function["args"]; ok {
			function["arguments"] = args
			delete(function, "args")
		} else if args, ok := call["input"]; ok {
			function["arguments"] = args
		}
	}
	if args := function["arguments"]; args != nil {
		if _, ok := args.(string); !ok {
			function["arguments"] = encodeToolArguments(args)
		}
	} else {
		function["arguments"] = ""
	}
}

func adaptChatMessageTools(message map[string]any, advertised []advertisedTool, extractContent bool) []string {
	if message == nil {
		return nil
	}
	liftChatMessageToolCalls(message)
	var notes []string
	if extractContent && len(anySlice(message["tool_calls"])) == 0 {
		if calls, rest, ok := extractToolCallsFromText(chatMessageText(message["content"])); ok {
			message["tool_calls"] = calls
			if rest == "" {
				message["content"] = ""
			} else {
				message["content"] = rest
			}
			notes = append(notes, "content-tool-call")
		}
	}
	for _, raw := range anySlice(message["tool_calls"]) {
		call, _ := raw.(map[string]any)
		notes = append(notes, adaptChatToolCall(call, advertised)...)
	}
	return notes
}

func adaptChatToolCall(call map[string]any, advertised []advertisedTool) []string {
	liftChatToolCallObject(call)
	function, _ := call["function"].(map[string]any)
	if function == nil {
		return nil
	}
	emitted := stringValue(function["name"])
	arguments := encodeToolArguments(function["arguments"])
	resolved, rewritten, notes := adaptResolvedCall(emitted, arguments, advertised)
	if resolved != "" {
		function["name"] = resolved
		if stringValue(call["name"]) != "" {
			call["name"] = resolved
		}
	}
	if rewritten != arguments {
		function["arguments"] = rewritten
	}
	return notes
}

func backfillChatToolResultNames(messages []any) []string {
	names := map[string]string{}
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if !strings.EqualFold(stringValue(message["role"]), "assistant") {
			continue
		}
		for _, rawCall := range anySlice(message["tool_calls"]) {
			call, _ := rawCall.(map[string]any)
			id := stringValue(call["id"])
			function, _ := call["function"].(map[string]any)
			name := stringValue(function["name"])
			if name == "" {
				name = stringValue(call["name"])
			}
			if id != "" && name != "" {
				names[id] = name
			}
		}
	}
	var notes []string
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if !strings.EqualFold(stringValue(message["role"]), "tool") {
			continue
		}
		id := stringValue(message["tool_call_id"])
		name := names[id]
		if name == "" || stringValue(message["name"]) == name {
			continue
		}
		message["name"] = name
		notes = append(notes, "tool-result-name")
	}
	return notes
}

func adaptResponsesItems(value any, advertised []advertisedTool) []string {
	var notes []string
	for _, raw := range anySlice(value) {
		item, _ := raw.(map[string]any)
		notes = append(notes, adaptResponsesItem(item, advertised)...)
	}
	return notes
}

func adaptResponsesItem(item map[string]any, advertised []advertisedTool) []string {
	if item == nil || !strings.EqualFold(strings.TrimSpace(stringValue(item["type"])), "function_call") {
		return nil
	}
	emitted := stringValue(item["name"])
	arguments := encodeToolArguments(item["arguments"])
	if _, ok := item["arguments"].(string); !ok && item["arguments"] != nil {
		item["arguments"] = arguments
	}
	resolved, rewritten, notes := adaptResolvedCall(emitted, arguments, advertised)
	if resolved != "" {
		item["name"] = resolved
	}
	if rewritten != arguments {
		item["arguments"] = rewritten
	}
	return notes
}

func adaptMessagesContent(value any, advertised []advertisedTool) []string {
	var notes []string
	for _, raw := range anySlice(value) {
		block, _ := raw.(map[string]any)
		notes = append(notes, adaptMessagesToolUse(block, advertised)...)
	}
	return notes
}

func adaptMessagesToolUse(block map[string]any, advertised []advertisedTool) []string {
	if block == nil || !strings.EqualFold(strings.TrimSpace(stringValue(block["type"])), "tool_use") {
		return nil
	}
	emitted := stringValue(block["name"])
	arguments := encodeToolArguments(block["input"])
	resolved, rewritten, notes := adaptResolvedCall(emitted, arguments, advertised)
	if resolved != "" {
		block["name"] = resolved
	}
	if rewritten != arguments {
		var next map[string]any
		if json.Unmarshal([]byte(rewritten), &next) == nil {
			block["input"] = next
		}
	}
	return notes
}

func extractToolCallsFromText(content string) ([]any, string, bool) {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return nil, content, false
	}
	var calls []any
	rest := content
	for {
		span, ok := nextToolCallMarkup(rest)
		if !ok {
			break
		}
		call, parsed := parseExtractedToolCall(span.inner, span.open)
		if !parsed {
			break
		}
		calls = append(calls, call)
		rest = strings.TrimSpace(rest[:span.start] + rest[span.end:])
	}
	if len(calls) == 0 {
		return nil, content, false
	}
	return calls, rest, true
}

type toolCallMarkup struct {
	start, end int
	inner      string
	open       string
}

func nextToolCallMarkup(src string) (toolCallMarkup, bool) {
	best := toolCallMarkup{start: -1}
	consider := func(span toolCallMarkup, ok bool) {
		if !ok {
			return
		}
		if best.start < 0 || span.start < best.start {
			best = span
		}
	}
	consider(taggedXMLBlock(src, "tool_call"))
	consider(taggedXMLBlock(src, "tool_calls"))
	consider(taggedXMLBlock(src, "function_call"))
	consider(taggedXMLBlock(src, "invoke"))
	if idx := indexFold(src, "```tool_call"); idx >= 0 {
		from := idx + len("```tool_call")
		relEnd := strings.Index(src[from:], "```")
		if relEnd >= 0 {
			consider(toolCallMarkup{
				start: idx,
				end:   from + relEnd + 3,
				inner: src[from : from+relEnd],
				open:  "```tool_call",
			}, true)
		}
	}
	if best.start < 0 {
		return toolCallMarkup{}, false
	}
	return best, true
}

func taggedXMLBlock(src, tag string) (toolCallMarkup, bool) {
	openPrefix := "<" + tag
	idx := indexFold(src, openPrefix)
	if idx < 0 {
		return toolCallMarkup{}, false
	}
	after := idx + len(openPrefix)
	if after > len(src) {
		return toolCallMarkup{}, false
	}
	if after < len(src) {
		switch src[after] {
		case '>', ' ', '\t', '\n', '\r', '/':
		default:
			return toolCallMarkup{}, false
		}
	}
	gt := strings.IndexByte(src[idx:], '>')
	if gt < 0 {
		return toolCallMarkup{}, false
	}
	open := src[idx : idx+gt+1]
	close := "</" + tag + ">"
	from := idx + len(open)
	relEnd := indexFold(src[from:], close)
	if relEnd < 0 {
		return toolCallMarkup{}, false
	}
	return toolCallMarkup{
		start: idx,
		end:   from + relEnd + len(close),
		inner: src[from : from+relEnd],
		open:  open,
	}, true
}

func parseExtractedToolCall(inner, open string) (map[string]any, bool) {
	inner = strings.TrimSpace(inner)
	if inner == "" {
		return nil, false
	}
	if strings.HasPrefix(inner, "{") {
		var obj map[string]any
		if json.Unmarshal([]byte(inner), &obj) != nil {
			return nil, false
		}
		name := firstString(obj, "name", "function", "tool")
		args := obj["arguments"]
		if args == nil {
			args = obj["parameters"]
		}
		if args == nil {
			args = obj["input"]
		}
		if args == nil {
			args = obj["args"]
		}
		if name == "" {
			return nil, false
		}
		call := map[string]any{
			"id":   compatID("call"),
			"type": "function",
			"function": map[string]any{
				"name":      name,
				"arguments": encodeToolArguments(valueOr(args, map[string]any{})),
			},
		}
		return call, true
	}
	name := xmlAttrValue(inner, "function")
	if name == "" {
		name = xmlTagValue(inner, "function")
	}
	if name == "" {
		name = xmlNamedAttr(open, "name")
	}
	if name == "" {
		name = xmlNamedAttr(inner, "name")
	}
	if name == "" {
		return nil, false
	}
	args := map[string]any{}
	remaining := inner
	for {
		key, value, next, ok := nextXMLParameter(remaining)
		if !ok {
			break
		}
		args[key] = value
		remaining = next
	}
	call := map[string]any{
		"id":   compatID("call"),
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": encodeToolArguments(args),
		},
	}
	return call, true
}

func xmlAttrValue(src, tag string) string {
	prefix := "<" + tag + "="
	idx := indexFold(src, prefix)
	if idx < 0 {
		return ""
	}
	rest := src[idx+len(prefix):]
	end := strings.IndexAny(rest, " >")
	if end < 0 {
		return strings.Trim(rest, `"'`)
	}
	return strings.Trim(rest[:end], `"'`)
}

func xmlNamedAttr(src, key string) string {
	needle := key + "="
	idx := indexFold(src, needle)
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(src[idx+len(needle):])
	if rest == "" {
		return ""
	}
	switch rest[0] {
	case '"', '\'':
		quote := rest[0]
		end := strings.IndexByte(rest[1:], quote)
		if end < 0 {
			return ""
		}
		return rest[1 : 1+end]
	default:
		end := strings.IndexAny(rest, " \t\r\n>/")
		if end < 0 {
			return strings.Trim(rest, `"'`)
		}
		return strings.Trim(rest[:end], `"'`)
	}
}

func xmlTagValue(src, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	start := indexFold(src, open)
	end := indexFold(src, close)
	if start < 0 || end < 0 || end <= start {
		return ""
	}
	return strings.TrimSpace(src[start+len(open) : end])
}

func nextXMLParameter(src string) (string, string, string, bool) {
	if key, value, next, ok := nextXMLParameterEq(src); ok {
		return key, value, next, true
	}
	return nextXMLParameterNamed(src)
}

func nextXMLParameterEq(src string) (string, string, string, bool) {
	const open = "<parameter="
	start := indexFold(src, open)
	if start < 0 {
		return "", "", src, false
	}
	rest := src[start+len(open):]
	gt := strings.IndexByte(rest, '>')
	if gt < 0 {
		return "", "", src, false
	}
	key := strings.Trim(strings.TrimSpace(rest[:gt]), `"'`)
	body := rest[gt+1:]
	close := indexFold(body, "</parameter>")
	if close < 0 {
		return "", "", src, false
	}
	value := strings.TrimSpace(body[:close])
	return key, value, body[close+len("</parameter>"):], key != ""
}

func nextXMLParameterNamed(src string) (string, string, string, bool) {
	const open = "<parameter"
	start := indexFold(src, open)
	if start < 0 {
		return "", "", src, false
	}
	rest := src[start+len(open):]
	if rest != "" {
		switch rest[0] {
		case ' ', '\t', '\n', '\r':
		default:
			return "", "", src, false
		}
	}
	gt := strings.IndexByte(rest, '>')
	if gt < 0 {
		return "", "", src, false
	}
	key := xmlNamedAttr(rest[:gt], "name")
	body := rest[gt+1:]
	close := indexFold(body, "</parameter>")
	if close < 0 {
		return "", "", src, false
	}
	value := strings.TrimSpace(body[:close])
	return key, value, body[close+len("</parameter>"):], key != ""
}

func indexFold(src, token string) int {
	return strings.Index(strings.ToLower(src), strings.ToLower(token))
}

func uniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
