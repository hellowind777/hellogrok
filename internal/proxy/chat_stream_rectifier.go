package proxy

import (
	"fmt"
	"sort"
	"strings"
)

// chatToolRectifier reassembles Chat Completions tool-call deltas before they
// reach Grok Build. Native chat streams are forwarded frame-by-frame; a
// provider that repeats `"name":""` on later chunks, or a per-frame argument
// rewrite on incomplete JSON, makes Grok Build's last-write-wins accumulator
// drop the tool name and report NotFound.
type chatToolRectifier struct {
	advertised  []advertisedTool
	searchAlias string
	tools       map[int]*chatToolRectState
	envelope    map[string]any
	choiceIndex int
	flushed     bool
	thoughts    thoughtGate
	thinks      inlineThinkState
	late        []string
	lateSeen    map[int]bool
}

type chatToolRectState struct {
	index int
	id    string
	name  string
	args  strings.Builder
}

func newChatToolRectifier(advertised []advertisedTool, searchAlias string) *chatToolRectifier {
	return &chatToolRectifier{
		advertised:  advertised,
		searchAlias: searchAlias,
		tools:       map[int]*chatToolRectState{},
	}
}

func (r *chatToolRectifier) ingest(root map[string]any) ([]map[string]any, []string) {
	if r == nil || root == nil {
		return []map[string]any{root}, nil
	}
	if root["error"] != nil {
		frames, notes := r.flush()
		return append(frames, root), r.drainLate(notes)
	}
	r.rememberEnvelope(root)
	passthrough := cloneMap(root)
	finish := false
	for _, raw := range anySlice(passthrough["choices"]) {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}
		if reason := strings.TrimSpace(stringValue(choice["finish_reason"])); reason != "" {
			finish = true
		}
		if message, _ := choice["message"].(map[string]any); message != nil {
			liftChatWireDialect(message)
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		liftChatWireDialect(delta)
		content := chatMessageText(delta["content"])
		if content != "" {
			reasoning, text, hold := r.thinks.feed(content)
			if hold {
				delete(delta, "content")
			} else {
				mergeChatThinkDelta(delta, reasoning, text)
			}
		}
		calls := anySlice(delta["tool_calls"])
		if len(calls) > 0 || finish {
			reasoning, text := r.thinks.flush()
			mergeChatThinkDelta(delta, reasoning, text)
		}
		if len(calls) == 0 {
			continue
		}
		choiceIndex := 0
		if _, present, valid := optionalCanonicalToken(choice, "index"); present && valid {
			choiceIndex = numberInt(choice["index"])
		}
		for position, rawCall := range calls {
			r.observe(choiceIndex, position, rawCall)
		}
		delete(delta, "tool_calls")
		if chatDeltaEmpty(delta) {
			choice["delta"] = map[string]any{}
		}
	}
	prefix := r.takeReasoningPrefix(passthrough)
	if finish {
		// Tool frames stay buffered until the stream terminal: Grok Build's
		// chat accumulator reads the whole stream and ignores chunk order, so
		// emitting them after the stop chunk keeps relays that send
		// finish_reason before the last argument fragments lossless.
		frames := r.flushThinks()
		out := append([]map[string]any{}, prefix...)
		out = append(out, frames...)
		if chatChunkHasClientPayload(passthrough) {
			out = append(out, passthrough)
		}
		return out, r.drainLate(nil)
	}
	if len(prefix) > 0 {
		if chatChunkHasClientPayload(passthrough) {
			return append(prefix, passthrough), r.drainLate(nil)
		}
		return prefix, r.drainLate(nil)
	}
	if chatChunkHasClientPayload(passthrough) {
		return []map[string]any{passthrough}, r.drainLate(nil)
	}
	return nil, r.drainLate(nil)
}

// takeReasoningPrefix makes native Chat Completions match Grok Build's
// conversation order [Reasoning, Assistant]. The TUI finishes the Thought
// block on the first AgentMessageChunk, then a later reasoning delta opens a
// second Thought under the reply. Official grok never emits that pattern.
func (r *chatToolRectifier) takeReasoningPrefix(root map[string]any) []map[string]any {
	if r == nil || root == nil {
		return nil
	}
	var prefix []map[string]any
	for _, raw := range anySlice(root["choices"]) {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}
		obj := chatReasoningCarrier(choice)
		if obj == nil {
			continue
		}
		liftChatWireDialect(obj)
		content := strings.TrimSpace(chatMessageText(obj["content"]))
		cleaned, keep := r.thoughts.accept(stringValue(obj["reasoning_content"]))
		if content != "" {
			r.thoughts.noteText(content)
			if keep {
				prefix = append(prefix, r.reasoningFrame(root, choice, cleaned))
			}
			clearChatReasoning(obj)
			continue
		}
		if keep {
			writeChatReasoning(obj, cleaned)
		} else {
			clearChatReasoning(obj)
		}
	}
	return prefix
}

func (r *chatToolRectifier) contentFrame(root map[string]any, text string) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": r.choiceIndex,
			"delta": map[string]any{
				"content": text,
			},
		}},
	}
	copyIfPresent(frame, root, "id", "object", "created", "model")
	return frame
}

func (r *chatToolRectifier) reasoningFrame(root, choice map[string]any, text string) map[string]any {
	index := r.choiceIndex
	if _, present, valid := optionalCanonicalToken(choice, "index"); present && valid {
		index = numberInt(choice["index"])
	}
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": index,
			"delta": map[string]any{
				"reasoning_content": text,
			},
		}},
	}
	copyIfPresent(frame, root, "id", "object", "created", "model")
	return frame
}

func chatReasoningCarrier(choice map[string]any) map[string]any {
	if choice == nil {
		return nil
	}
	if delta, _ := choice["delta"].(map[string]any); delta != nil {
		return delta
	}
	message, _ := choice["message"].(map[string]any)
	return message
}

func (r *chatToolRectifier) observe(choiceIndex, position int, raw any) {
	call, _ := raw.(map[string]any)
	if call == nil {
		return
	}
	liftChatToolCallDelta(call)
	index := position
	if value, present, valid := optionalCanonicalToken(call, "index"); present && valid {
		index = int(value)
	}
	state := r.tools[index]
	if state == nil {
		state = &chatToolRectState{index: index}
		r.tools[index] = state
	}
	r.choiceIndex = choiceIndex
	if id := strings.TrimSpace(stringValue(call["id"])); id != "" && state.id == "" {
		state.id = id
	}
	function, _ := call["function"].(map[string]any)
	if name := strings.TrimSpace(stringValue(function["name"])); name != "" {
		state.name = name
	}
	fragment := encodeToolArguments(function["arguments"])
	if fragment != "" {
		state.args.WriteString(fragment)
	}
	if r.flushed && (strings.TrimSpace(state.name) != "" || state.args.Len() > 0) {
		r.noteLateDiscard(index)
	}
}

// noteLateDiscard records tool-call deltas that arrive after the rectifier
// already flushed at finish_reason: a relay emitting the stop signal before
// the last argument fragments loses them here, leaving the client with
// tail-truncated arguments. The note makes that relay defect visible in the
// proxy log instead of surfacing only as a Grok Build parse failure.
func (r *chatToolRectifier) noteLateDiscard(index int) {
	if r.lateSeen[index] {
		return
	}
	if r.lateSeen == nil {
		r.lateSeen = map[int]bool{}
	}
	r.lateSeen[index] = true
	r.late = append(r.late, fmt.Sprintf("late-tool-deltas-discarded(index=%d)", index))
}

func (r *chatToolRectifier) drainLate(notes []string) []string {
	if len(r.late) == 0 {
		return notes
	}
	notes = append(notes, r.late...)
	r.late = nil
	return notes
}

func (r *chatToolRectifier) rememberEnvelope(root map[string]any) {
	if root == nil {
		return
	}
	envelope := map[string]any{}
	copyIfPresent(envelope, root, "id", "object", "created", "model")
	if len(envelope) == 0 {
		return
	}
	r.envelope = envelope
}

// flushThinks emits any held inline-think content without touching buffered
// tool calls. It runs at finish_reason so held reasoning/text still precedes
// the stop chunk, while tool frames wait for the stream terminal.
func (r *chatToolRectifier) flushThinks() []map[string]any {
	if r == nil {
		return nil
	}
	template := r.envelope
	if template == nil {
		template = map[string]any{"object": "chat.completion.chunk"}
	}
	var frames []map[string]any
	reasoning, text := r.thinks.flush()
	if reasoning != "" {
		if cleaned, keep := r.thoughts.accept(reasoning); keep {
			frames = append(frames, r.reasoningFrame(template, map[string]any{"index": r.choiceIndex}, cleaned))
		}
	}
	if text != "" {
		r.thoughts.noteText(text)
		frames = append(frames, r.contentFrame(template, text))
	}
	return frames
}

func (r *chatToolRectifier) flush() ([]map[string]any, []string) {
	if r == nil {
		return nil, nil
	}
	frames := r.flushThinks()
	template := r.envelope
	if template == nil {
		template = map[string]any{"object": "chat.completion.chunk"}
	}
	if r.flushed || len(r.tools) == 0 {
		return frames, nil
	}
	r.flushed = true
	indexes := make([]int, 0, len(r.tools))
	for index := range r.tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	seenIDs := map[string]struct{}{}
	var notes []string
	for _, index := range indexes {
		state := r.tools[index]
		if state == nil {
			continue
		}
		if strings.TrimSpace(state.id) == "" && strings.TrimSpace(state.name) == "" && state.args.Len() == 0 {
			continue
		}
		if state.id == "" {
			state.id = compatID("call")
		}
		if _, taken := seenIDs[state.id]; taken {
			// Gateways that reuse one call id for every tool_call would
			// poison the next round's tool history; assign a unique id.
			state.id = compatID("call")
		}
		seenIDs[state.id] = struct{}{}
		name := restoreStreamToolName(state.name, r.searchAlias)
		args := state.args.String()
		resolved, rewritten, callNotes := adaptResolvedCall(name, args, r.advertised)
		notes = append(notes, callNotes...)
		if strings.TrimSpace(resolved) == "" {
			resolved = name
		}
		if strings.TrimSpace(resolved) == "" {
			// An unnamed call that shape inference could not resolve reaches Grok
			// Build with an empty name and dies as NotFound, a terminal error the
			// model never sees fed back. Route it onto a self-healing path instead:
			// hand the raw arguments to a real tool so the next turn returns them
			// in a tool_result and the model regenerates the call.
			resolved, rewritten = fallbackUnresolvedCall(rewritten, r.advertised)
			notes = append(notes, fmt.Sprintf("unresolved-name-routed(name=%s)", resolved))
		}
		if len(parseToolArguments(rewritten)) == 0 {
			// A call that accumulated no argument fragments at all cannot
			// satisfy a schema with required properties: Grok Build would
			// dispatch it as an empty object and report a guaranteed parse
			// failure. Drop it here and keep the defect visible in the log.
			if tool, ok := advertisedToolNamed(r.advertised, resolved); ok && tool.HasRequired {
				notes = append(notes, fmt.Sprintf("empty-args-discarded(name=%s)", tool.Name))
				continue
			}
		}
		frames = append(frames, r.toolFrame(template, map[string]any{
			"index": index,
			"id":    state.id,
			"type":  "function",
			"function": map[string]any{
				"name":      resolved,
				"arguments": "",
			},
		}))
		if rewritten != "" {
			frames = append(frames, r.toolFrame(template, map[string]any{
				"index": index,
				"function": map[string]any{
					"arguments": rewritten,
				},
			}))
		}
	}
	return frames, uniqueStrings(notes)
}

// fallbackUnresolvedCall picks a deterministic advertised tool to carry an
// unnamed, shape-unresolvable call so Grok Build parses it and feeds the error
// back to the model. Parseable arguments go to run_terminal_command (a shell
// echo of the raw text); corrupt arguments go to read_file (the raw text as the
// path). Both fail cleanly and return the original arguments to the model,
// which is what makes the retry possible. Returns empty when neither tool is
// advertised, leaving the call untouched.
func fallbackUnresolvedCall(arguments string, advertised []advertisedTool) (string, string) {
	carrier := "run_terminal_command"
	key := "command"
	if !jsonObjectComplete(arguments) {
		carrier = "read_file"
		key = "target_file"
	}
	if !advertisedHas(advertised, carrier) {
		return "", arguments
	}
	raw := strings.TrimSpace(arguments)
	if raw == "" {
		raw = "(no arguments)"
	}
	return advertisedCanonicalName(advertised, carrier), encodeToolArguments(map[string]any{key: raw})
}

func (r *chatToolRectifier) toolFrame(template map[string]any, call map[string]any) map[string]any {
	frame := map[string]any{
		"object": "chat.completion.chunk",
		"choices": []any{map[string]any{
			"index": r.choiceIndex,
			"delta": map[string]any{
				"tool_calls": []any{call},
			},
		}},
	}
	copyIfPresent(frame, template, "id", "object", "created", "model")
	return frame
}

func chatDeltaEmpty(delta map[string]any) bool {
	if len(delta) == 0 {
		return true
	}
	for _, key := range []string{"role", "tool_call_id"} {
		if strings.TrimSpace(stringValue(delta[key])) != "" {
			return false
		}
	}
	if chatMessageText(delta["content"]) != "" {
		return false
	}
	if chatThoughtText(delta) != "" {
		return false
	}
	if len(anySlice(delta["tool_calls"])) > 0 {
		return false
	}
	return true
}

func chatChunkHasClientPayload(root map[string]any) bool {
	if root == nil {
		return false
	}
	if _, present := root["usage"]; present {
		return true
	}
	for _, raw := range anySlice(root["choices"]) {
		choice, _ := raw.(map[string]any)
		if choice == nil {
			continue
		}
		if strings.TrimSpace(stringValue(choice["finish_reason"])) != "" {
			return true
		}
		delta, _ := choice["delta"].(map[string]any)
		if !chatDeltaEmpty(delta) {
			return true
		}
	}
	return false
}

func liftChatToolCallDelta(call map[string]any) {
	if call == nil {
		return
	}
	function, _ := call["function"].(map[string]any)
	if function == nil {
		function = map[string]any{}
		call["function"] = function
	}
	if strings.TrimSpace(stringValue(function["name"])) == "" {
		if name := firstString(call, "name"); name != "" {
			function["name"] = name
		}
	}
	if function["arguments"] == nil {
		if args, ok := call["arguments"]; ok {
			function["arguments"] = args
		}
	}
	if args := function["arguments"]; args != nil {
		if _, ok := args.(string); !ok {
			function["arguments"] = encodeToolArguments(args)
		}
	}
}
