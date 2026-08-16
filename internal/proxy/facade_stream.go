package proxy

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

var (
	errSSEStreamComplete  = errors.New("SSE stream complete")
	heartbeatNameReplacer = strings.NewReplacer("-", "", "_", "", ".", "", " ", "")
)

type translatedStreamWriter struct {
	w           http.ResponseWriter
	flusher     http.Flusher
	sequence    int
	writeFailed bool
}

func beginTranslatedStream(w http.ResponseWriter, response *http.Response) (*translatedStreamWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("stream unsupported")
	}
	copySafeResponseHeaders(w.Header(), response.Header)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(response.StatusCode)
	flusher.Flush()
	return &translatedStreamWriter{w: w, flusher: flusher}, nil
}

func (w *translatedStreamWriter) emit(typ string, values map[string]any) error {
	values["type"] = typ
	values["sequence_number"] = w.sequence
	data, err := json.Marshal(values)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", typ, err)
	}
	if _, err := fmt.Fprintf(w.w, "event: %s\ndata: %s\n\n", typ, data); err != nil {
		w.writeFailed = true
		return err
	}
	w.sequence++
	w.flusher.Flush()
	return nil
}

func (w *translatedStreamWriter) emitHeartbeat() error {
	if _, err := io.WriteString(w.w, ": keepalive\n\n"); err != nil {
		w.writeFailed = true
		return err
	}
	w.flusher.Flush()
	return nil
}

func (w *translatedStreamWriter) emitStart(route config.Route, request facadeRequest, responseID string, createdAt int64) error {
	created := translatedResponse(route, request, canonicalResult{}, responseID, createdAt)
	created["completed_at"] = nil
	created["status"] = "in_progress"
	created["output"] = []any{}
	created["usage"] = nil
	if err := w.emit("response.created", map[string]any{"response": created}); err != nil {
		return err
	}
	return w.emit("response.in_progress", map[string]any{"response": cloneMap(created)})
}

func (w *translatedStreamWriter) emitStreamError(message string) {
	if w.writeFailed {
		return
	}
	writeResponsesStreamError(w.w, w.flusher, w.sequence, message)
}

func translatedResponse(route config.Route, request facadeRequest, result canonicalResult, responseID string, createdAt int64) map[string]any {
	response := canonicalResponse(route, request, result)
	response["id"] = responseID
	response["created_at"] = createdAt
	return response
}

func scanSSEPayloads(reader io.Reader, consume func([]string, []byte) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxSSEEventBytes)
	frame := make([]string, 0, 4)
	frameBytes := 0
	flush := func() error {
		if len(frame) == 0 {
			return nil
		}
		payload, ok := sseFramePayload(frame)
		lines := append([]string(nil), frame...)
		frame = frame[:0]
		frameBytes = 0
		if !ok {
			if isPrivateSSEHeartbeat(lines, "") {
				return consume(lines, nil)
			}
			return nil
		}
		return consume(lines, []byte(payload))
	}
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if frameBytes+len(line)+1 > maxSSEEventBytes {
			return fmt.Errorf("SSE event exceeds 16 MiB")
		}
		frame = append(frame, line)
		frameBytes += len(line) + 1
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return flush()
}

func isPrivateSSEHeartbeat(lines []string, payload string) bool {
	eventName := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(strings.TrimSpace(line), ":") {
			comment := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), ":"))
			if isHeartbeatName(comment) {
				return true
			}
		}
	}

	trimmed := strings.TrimSpace(payload)
	var value any
	if json.Unmarshal([]byte(trimmed), &value) == nil {
		switch typed := value.(type) {
		case map[string]any:
			for _, key := range []string{"type", "event"} {
				if isHeartbeatName(stringValue(typed[key])) {
					return true
				}
			}
		case string:
			return isHeartbeatName(typed)
		}
	}
	if isHeartbeatName(trimmed) {
		return true
	}
	return isHeartbeatName(eventName)
}

func isHeartbeatName(value string) bool {
	normalized := heartbeatNameReplacer.Replace(strings.ToLower(strings.TrimSpace(value)))
	switch normalized {
	case "keepalive", "heartbeat", "ping", "responsekeepalive", "responseheartbeat", "responseping":
		return true
	default:
		return false
	}
}

type messagesStreamBlock struct {
	kind        string
	outputIndex int
	item        map[string]any
	native      map[string]any
	text        strings.Builder
	arguments   strings.Builder
	signature   strings.Builder
	annotations []any
	targetID    string
	done        bool
}

type messagesStreamState struct {
	writer       *translatedStreamWriter
	route        config.Route
	request      facadeRequest
	responseID   string
	createdAt    int64
	messageID    string
	model        string
	blocks       map[int]*messagesStreamBlock
	webCalls     map[string]*messagesStreamBlock
	output       []any
	usageStart   map[string]any
	usage        map[string]any
	stopReason   string
	sawStart     bool
	terminal     bool
	hadWebSearch bool
	allURLs      []string
	textURLs     []string
}

func newMessagesStreamState(writer *translatedStreamWriter, route config.Route, request facadeRequest) *messagesStreamState {
	return &messagesStreamState{
		writer:     writer,
		route:      route,
		request:    request,
		responseID: compatID("resp"),
		createdAt:  time.Now().Unix(),
		blocks:     map[int]*messagesStreamBlock{},
		webCalls:   map[string]*messagesStreamBlock{},
		usage:      map[string]any{},
	}
}

func (s *messagesStreamState) handle(payload []byte) error {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || trimmed == "[DONE]" || s.terminal {
		return nil
	}
	event, err := decodeJSONMap(payload)
	if err != nil {
		return fmt.Errorf("decode Messages SSE event: %w", err)
	}
	normalizeMessagesStreamRequiredFields(event)
	switch stringValue(event["type"]) {
	case "ping":
		return nil
	case "error":
		errorBody, _ := event["error"].(map[string]any)
		code := firstString(errorBody, "type", "code")
		if code == "" {
			code = "upstream_error"
		}
		message := firstString(errorBody, "message")
		if message == "" {
			message = "upstream Messages stream failed"
		}
		s.terminal = true
		return s.writer.emit("error", map[string]any{"code": code, "message": message, "param": nil})
	case "message_start":
		message, _ := event["message"].(map[string]any)
		if err := validateMessagesEnvelope(message); err != nil {
			return err
		}
		s.sawStart = true
		s.messageID = stringValue(message["id"])
		s.model = stringValue(message["model"])
		s.observeStartUsage(message["usage"])
		for index, raw := range anySlice(message["content"]) {
			block, _ := raw.(map[string]any)
			if err := s.startBlock(index, block); err != nil {
				return err
			}
			if err := s.stopBlock(index); err != nil {
				return err
			}
		}
	case "content_block_start":
		index := numberInt(event["index"])
		block, _ := event["content_block"].(map[string]any)
		if block == nil {
			return fmt.Errorf("Messages content_block_start content_block must be an object")
		}
		return s.startBlock(index, block)
	case "content_block_delta":
		index := numberInt(event["index"])
		delta, _ := event["delta"].(map[string]any)
		if delta == nil {
			return fmt.Errorf("Messages content_block_delta delta must be an object")
		}
		return s.deltaBlock(index, delta)
	case "content_block_stop":
		return s.stopBlock(numberInt(event["index"]))
	case "message_delta":
		delta, _ := event["delta"].(map[string]any)
		if delta != nil {
			s.stopReason = stringValue(delta["stop_reason"])
		}
		s.observeDeltaUsage(event["usage"])
	case "message_stop":
		if !s.sawStart {
			return fmt.Errorf("Messages stream ended before message_start")
		}
		return s.finish()
	default:
		return fmt.Errorf("unsupported Messages SSE event type %q", stringValue(event["type"]))
	}
	return nil
}

func (s *messagesStreamState) startBlock(index int, raw map[string]any) error {
	if _, exists := s.blocks[index]; exists {
		return fmt.Errorf("Messages content block %d started more than once", index)
	}
	block := &messagesStreamBlock{kind: stringValue(raw["type"]), outputIndex: -1, native: cloneMap(raw)}
	s.blocks[index] = block
	s.allURLs = mergeUniqueStrings(s.allURLs, urlsFromJSON(raw)...)

	switch block.kind {
	case "text":
		block.annotations = citationsToAnnotations(raw["citations"])
		item := messageItem("", block.annotations)
		item["status"] = "in_progress"
		block.item, block.outputIndex = item, s.appendItem(item)
		added := cloneMap(item)
		added["content"] = []any{}
		if err := s.writer.emit("response.output_item.added", map[string]any{"output_index": block.outputIndex, "item": added}); err != nil {
			return err
		}
		part := map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}
		if err := s.writer.emit("response.content_part.added", map[string]any{"output_index": block.outputIndex, "item_id": stringValue(item["id"]), "content_index": 0, "part": part}); err != nil {
			return err
		}
		return s.appendMessageText(block, stringValue(raw["text"]))
	case "thinking":
		block.signature.WriteString(stringValue(raw["signature"]))
		item := reasoningItem("", "")
		item["status"] = "in_progress"
		block.item, block.outputIndex = item, s.appendItem(item)
		added := cloneMap(item)
		added["content"] = []any{}
		if err := s.writer.emit("response.output_item.added", map[string]any{"output_index": block.outputIndex, "item": added}); err != nil {
			return err
		}
		if err := s.writer.emit("response.content_part.added", map[string]any{"output_index": block.outputIndex, "item_id": stringValue(item["id"]), "content_index": 0, "part": map[string]any{"type": "reasoning_text", "text": ""}}); err != nil {
			return err
		}
		return s.appendThinking(block, stringValue(raw["thinking"]))
	case "redacted_thinking":
		item := reasoningItem("", stringValue(raw["data"]))
		item["status"] = "in_progress"
		block.item, block.outputIndex = item, s.appendItem(item)
		return s.writer.emit("response.output_item.added", map[string]any{"output_index": block.outputIndex, "item": cloneMap(item)})
	case "tool_use":
		wireName := stringValue(raw["name"])
		name := restoreStreamToolName(wireName, s.request.ClientSearchAlias)
		item := functionCallItem(firstString(raw, "id"), name, "")
		item["status"] = "in_progress"
		block.item, block.outputIndex = item, s.appendItem(item)
		return s.writer.emit("response.output_item.added", map[string]any{"output_index": block.outputIndex, "item": cloneMap(item)})
	case "server_tool_use":
		if stringValue(raw["name"]) != "web_search" {
			return nil
		}
		input, _ := raw["input"].(map[string]any)
		item := webSearchItem(firstString(raw, "id"), firstString(input, "query", "q"), nil, "in_progress")
		block.item, block.outputIndex = item, s.appendItem(item)
		s.webCalls[stringValue(item["id"])] = block
		s.hadWebSearch = true
		if err := s.writer.emit("response.output_item.added", map[string]any{"output_index": block.outputIndex, "item": cloneMap(item)}); err != nil {
			return err
		}
		if err := s.writer.emit("response.web_search_call.in_progress", map[string]any{"output_index": block.outputIndex, "item_id": stringValue(item["id"])}); err != nil {
			return err
		}
		return s.writer.emit("response.web_search_call.searching", map[string]any{"output_index": block.outputIndex, "item_id": stringValue(item["id"])})
	case "web_search_tool_result":
		block.targetID = firstString(raw, "tool_use_id")
		return nil
	default:
		return nil
	}
}

func (s *messagesStreamState) deltaBlock(index int, delta map[string]any) error {
	block := s.blocks[index]
	if block == nil {
		return fmt.Errorf("Messages content block %d received a delta before start", index)
	}
	s.allURLs = mergeUniqueStrings(s.allURLs, urlsFromJSON(delta)...)
	switch stringValue(delta["type"]) {
	case "text_delta":
		return s.appendMessageText(block, stringValue(delta["text"]))
	case "thinking_delta":
		return s.appendThinking(block, stringValue(delta["thinking"]))
	case "signature_delta":
		block.signature.WriteString(stringValue(delta["signature"]))
	case "input_json_delta":
		partial := stringValue(delta["partial_json"])
		block.arguments.WriteString(partial)
		if block.kind == "tool_use" && partial != "" {
			return s.writer.emit("response.function_call_arguments.delta", map[string]any{"output_index": block.outputIndex, "item_id": stringValue(block.item["id"]), "delta": partial})
		}
	case "citations_delta", "citation_delta":
		urls := urlsFromJSON(delta)
		block.annotations = mergeAnnotations(block.annotations, urlsToAnnotations(urls))
		if block.native != nil {
			citations := anySlice(block.native["citations"])
			if citation := valueFirst(delta, "citation", "citations"); citation != nil {
				citations = append(citations, anySliceOrValue(citation)...)
				block.native["citations"] = citations
			}
		}
	default:
		// Unknown deltas are ignored so a provider extension cannot destroy text
		// that was already streamed.
	}
	return nil
}

func (s *messagesStreamState) appendMessageText(block *messagesStreamBlock, text string) error {
	if text == "" || block.item == nil {
		return nil
	}
	block.text.WriteString(text)
	return s.writer.emit("response.output_text.delta", map[string]any{
		"output_index": block.outputIndex, "item_id": stringValue(block.item["id"]),
		"content_index": 0, "delta": text, "logprobs": []any{},
	})
}

func (s *messagesStreamState) appendThinking(block *messagesStreamBlock, text string) error {
	if text == "" || block.item == nil {
		return nil
	}
	block.text.WriteString(text)
	return s.writer.emit("response.reasoning_text.delta", map[string]any{
		"output_index": block.outputIndex, "item_id": stringValue(block.item["id"]),
		"content_index": 0, "delta": text,
	})
}

func (s *messagesStreamState) stopBlock(index int) error {
	block := s.blocks[index]
	if block == nil {
		return fmt.Errorf("Messages content block %d stopped before start", index)
	}
	if block.done {
		return nil
	}
	block.done = true
	switch block.kind {
	case "text":
		text := block.text.String()
		s.textURLs = mergeUniqueStrings(s.textURLs, urlsFromText(text)...)
		block.native["text"] = text
		block.item["status"] = "completed"
		part := map[string]any{"type": "output_text", "text": text, "annotations": block.annotations, "logprobs": []any{}}
		block.item["content"] = []any{part}
		if err := s.writer.emit("response.output_text.done", map[string]any{"output_index": block.outputIndex, "item_id": stringValue(block.item["id"]), "content_index": 0, "text": text, "logprobs": []any{}}); err != nil {
			return err
		}
		if err := s.writer.emit("response.content_part.done", map[string]any{"output_index": block.outputIndex, "item_id": stringValue(block.item["id"]), "content_index": 0, "part": part}); err != nil {
			return err
		}
		return s.emitItemDone(block)
	case "thinking":
		text := block.text.String()
		block.native["thinking"] = text
		block.native["signature"] = block.signature.String()
		block.item["status"] = "completed"
		if text != "" {
			block.item["content"] = []any{map[string]any{"type": "reasoning_text", "text": text}}
		}
		if signature := block.signature.String(); signature != "" {
			block.item["encrypted_content"] = signature
		}
		if err := s.writer.emit("response.reasoning_text.done", map[string]any{"output_index": block.outputIndex, "item_id": stringValue(block.item["id"]), "content_index": 0, "text": text}); err != nil {
			return err
		}
		if err := s.writer.emit("response.content_part.done", map[string]any{"output_index": block.outputIndex, "item_id": stringValue(block.item["id"]), "content_index": 0, "part": map[string]any{"type": "reasoning_text", "text": text}}); err != nil {
			return err
		}
		return s.emitItemDone(block)
	case "redacted_thinking":
		block.item["status"] = "completed"
		return s.emitItemDone(block)
	case "tool_use":
		arguments := block.arguments.String()
		if arguments == "" {
			if input := block.native["input"]; input != nil {
				encoded, _ := json.Marshal(input)
				if string(encoded) != "{}" {
					arguments = string(encoded)
				}
			}
		} else {
			var input any
			if json.Unmarshal([]byte(arguments), &input) == nil {
				block.native["input"] = input
			}
		}
		if arguments == "" {
			arguments = "{}"
		}
		block.item["arguments"] = arguments
		block.item["status"] = "completed"
		if err := s.writer.emit("response.function_call_arguments.done", map[string]any{"output_index": block.outputIndex, "item_id": stringValue(block.item["id"]), "arguments": arguments}); err != nil {
			return err
		}
		return s.emitItemDone(block)
	case "server_tool_use":
		if block.item != nil && block.arguments.Len() > 0 {
			var input map[string]any
			if json.Unmarshal([]byte(block.arguments.String()), &input) == nil {
				block.native["input"] = input
				action, _ := block.item["action"].(map[string]any)
				action["query"] = firstString(input, "query", "q")
			}
		}
		return nil
	case "web_search_tool_result":
		return s.finishWebResult(block)
	default:
		return nil
	}
}

func (s *messagesStreamState) finishWebResult(result *messagesStreamBlock) error {
	call := s.webCalls[result.targetID]
	if call == nil {
		item := webSearchItem(result.targetID, s.request.SearchQuery, nil, "in_progress")
		call = &messagesStreamBlock{kind: "server_tool_use", item: item, outputIndex: s.appendItem(item), done: true}
		s.webCalls[stringValue(item["id"])] = call
		s.hadWebSearch = true
		if err := s.writer.emit("response.output_item.added", map[string]any{"output_index": call.outputIndex, "item": cloneMap(item)}); err != nil {
			return err
		}
		if err := s.writer.emit("response.web_search_call.in_progress", map[string]any{"output_index": call.outputIndex, "item_id": stringValue(item["id"])}); err != nil {
			return err
		}
		if err := s.writer.emit("response.web_search_call.searching", map[string]any{"output_index": call.outputIndex, "item_id": stringValue(item["id"])}); err != nil {
			return err
		}
	}
	sources, failed := messageSearchSources(result.native["content"])
	mergeWebSearchSources(call.item, sources)
	status := "completed"
	if failed {
		status = "failed"
	}
	call.item["status"] = status
	if !failed {
		action, _ := call.item["action"].(map[string]any)
		if len(anySlice(action["sources"])) == 0 {
			return nil
		}
	}
	return s.finishWebCall(call)
}

func (s *messagesStreamState) finishWebCall(call *messagesStreamBlock) error {
	if call.item == nil || call.kind == "web_search_emitted" {
		return nil
	}
	status := stringValue(call.item["status"])
	if status == "" || status == "in_progress" {
		call.item["status"] = "completed"
	}
	if err := s.writer.emit("response.web_search_call.completed", map[string]any{"output_index": call.outputIndex, "item_id": stringValue(call.item["id"])}); err != nil {
		return err
	}
	if err := s.writer.emit("response.output_item.done", map[string]any{"output_index": call.outputIndex, "item": cloneMap(call.item)}); err != nil {
		return err
	}
	// Keep the public item status valid while using a private marker on the block.
	call.done = true
	call.kind = "web_search_emitted"
	return nil
}

func (s *messagesStreamState) emitItemDone(block *messagesStreamBlock) error {
	return s.writer.emit("response.output_item.done", map[string]any{"output_index": block.outputIndex, "item": cloneMap(block.item)})
}

func (s *messagesStreamState) appendItem(item map[string]any) int {
	index := len(s.output)
	s.output = append(s.output, item)
	return index
}

func (s *messagesStreamState) observeStartUsage(value any) {
	usage, _ := value.(map[string]any)
	if len(usage) == 0 {
		return
	}
	s.usageStart = cloneMap(usage)
}

func (s *messagesStreamState) observeDeltaUsage(value any) {
	usage, _ := value.(map[string]any)
	if len(usage) == 0 {
		return
	}
	if completeMessagesUsage(usage) {
		s.usage = cloneMap(usage)
		return
	}
	if _, hasOutput, validOutput := optionalCanonicalToken(usage, "output_tokens"); !hasOutput || !validOutput {
		return
	}
	if len(s.usageStart) == 0 {
		return
	}

	// Messages defines input/cache usage on message_start and output usage on
	// message_delta. Combining only those two protocol phases mirrors Grok
	// Build's native consumer without merging unrelated partial snapshots.
	candidate := cloneMap(s.usageStart)
	delete(candidate, "total_tokens")
	for key, child := range usage {
		candidate[key] = child
	}
	if completeMessagesUsage(candidate) {
		s.usage = candidate
	}
}

func completeMessagesUsage(value any) bool {
	var result canonicalResult
	applyMessagesUsage(&result, value)
	return result.UsagePresent && result.LiveContextPresent
}

func (s *messagesStreamState) searchUsage() any {
	return []any{s.usageStart, s.usage}
}

func (s *messagesStreamState) finish() error {
	if s.terminal {
		return nil
	}
	for _, index := range sortedBlockIndexes(s.blocks) {
		block := s.blocks[index]
		if !block.done {
			if err := s.stopBlock(index); err != nil {
				return err
			}
		}
	}
	searchUsage := s.searchUsage()
	searchConfirmed := s.hadWebSearch || (s.request.HostedWebSearch && (len(s.allURLs) > 0 || positiveSearchUsage(searchUsage)))
	if searchConfirmed {
		s.allURLs = mergeUniqueStrings(s.allURLs, s.textURLs...)
	}
	if s.hadWebSearch {
		backfillResponseSearchSources(map[string]any{"output": s.output, "usage": searchUsage}, s.request.HostedWebSearch, s.request.SearchQuery)
	}
	for _, call := range s.webCalls {
		if call.kind == "web_search_emitted" {
			continue
		}
		if err := s.finishWebCall(call); err != nil {
			return err
		}
	}
	if !s.hadWebSearch && s.request.HostedWebSearch && (len(s.allURLs) > 0 || positiveSearchUsage(searchUsage)) {
		item := webSearchItem("", s.request.SearchQuery, urlsToSources(s.allURLs), "in_progress")
		call := &messagesStreamBlock{kind: "server_tool_use", item: item, outputIndex: s.appendItem(item)}
		if err := s.writer.emit("response.output_item.added", map[string]any{"output_index": call.outputIndex, "item": cloneMap(item)}); err != nil {
			return err
		}
		if err := s.writer.emit("response.web_search_call.in_progress", map[string]any{"output_index": call.outputIndex, "item_id": stringValue(item["id"])}); err != nil {
			return err
		}
		if err := s.writer.emit("response.web_search_call.searching", map[string]any{"output_index": call.outputIndex, "item_id": stringValue(item["id"])}); err != nil {
			return err
		}
		if err := s.finishWebCall(call); err != nil {
			return err
		}
	}
	// Some Messages-compatible gateways report only search usage and final-text
	// URLs. The inferred call above already has sources; mirror those URLs into
	// the terminal output_text annotations used by Build's client search path.
	backfillResponseSearchSources(map[string]any{"output": s.output, "usage": searchUsage}, s.request.HostedWebSearch, s.request.SearchQuery)

	result := canonicalResult{Output: s.output}
	applyMessagesUsage(&result, s.usage)
	if s.stopReason == "max_tokens" || s.stopReason == "model_context_window_exceeded" {
		result.IncompleteReason = "max_output_tokens"
	}
	terminal := translatedResponse(s.route, s.request, result, s.responseID, s.createdAt)
	if err := validateResponsesEnvelope(terminal); err != nil {
		return err
	}
	typ := "response.completed"
	if result.IncompleteReason != "" {
		typ = "response.incomplete"
	}
	s.terminal = true
	return s.writer.emit(typ, map[string]any{"response": terminal})
}

func (s *Server) streamMessagesSSE(w http.ResponseWriter, response *http.Response, route config.Route, request facadeRequest, started time.Time) {
	writer, err := beginTranslatedStream(w, response)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	modelObserver := newUpstreamModelObserver(request.Protocol)
	defer modelObserver.log(s.log, route)
	state := newMessagesStreamState(writer, route, request)
	if err := writer.emitStart(route, request, state.responseID, state.createdAt); err != nil {
		return
	}
	evidence := newSearchEvidence()
	heartbeats := 0
	streamErr := scanSSEPayloads(response.Body, func(lines []string, payload []byte) error {
		if isPrivateSSEHeartbeat(lines, string(payload)) {
			heartbeats++
			return writer.emitHeartbeat()
		}
		modelObserver.observeJSON(payload, false)
		if strings.TrimSpace(string(payload)) != "[DONE]" {
			evidence.observeJSON(payload)
		}
		if err := state.handle(payload); err != nil {
			return err
		}
		s.captureReasoningProvenance(route, state.output)
		if state.terminal {
			return errSSEStreamComplete
		}
		return nil
	})
	if errors.Is(streamErr, errSSEStreamComplete) {
		streamErr = nil
	}
	if streamErr != nil {
		s.log.Printf("UP channel=%s Messages SSE conversion error: %v", route.ChannelID, streamErr)
		writer.emitStreamError(upstreamStreamFailureMessage("Messages", streamErr))
	} else if !state.terminal {
		s.log.Printf("UP channel=%s Messages SSE ended without message_stop", route.ChannelID)
		writer.emitStreamError("upstream Messages stream ended without message_stop")
	}
	s.log.Printf("UP channel=%s Messages SSE done events=%d heartbeats=%d terminal=%t %s", route.ChannelID, writer.sequence, heartbeats, state.terminal, time.Since(started).Round(time.Millisecond))
	s.logSearchEvidence(route.ChannelID, request, evidence)
}

type chatToolStream struct {
	wireIndex   int
	outputIndex int
	item        map[string]any
	callID      string
	name        strings.Builder
	arguments   strings.Builder
	started     bool
}

type chatStreamState struct {
	writer       *translatedStreamWriter
	route        config.Route
	request      facadeRequest
	responseID   string
	createdAt    int64
	output       []any
	reasoning    *messagesStreamBlock
	message      *messagesStreamBlock
	tools        map[int]*chatToolStream
	search       *messagesStreamBlock
	usage        map[string]any
	finishReason string
	allURLs      []string
	sawChunk     bool
	terminal     bool
}

func newChatStreamState(writer *translatedStreamWriter, route config.Route, request facadeRequest) *chatStreamState {
	return &chatStreamState{
		writer: writer, route: route, request: request,
		responseID: compatID("resp"), createdAt: time.Now().Unix(),
		tools: map[int]*chatToolStream{}, usage: map[string]any{},
	}
}

func (s *chatStreamState) handle(payload []byte) error {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" || s.terminal {
		return nil
	}
	if trimmed == "[DONE]" {
		return s.finish()
	}
	root, err := decodeJSONMap(payload)
	if err != nil {
		return fmt.Errorf("decode Chat Completions SSE chunk: %w", err)
	}
	if errorBody, _ := root["error"].(map[string]any); errorBody != nil {
		code := firstString(errorBody, "code", "type")
		if code == "" {
			code = "upstream_error"
		}
		message := firstString(errorBody, "message")
		if message == "" {
			message = "upstream Chat Completions stream failed"
		}
		s.terminal = true
		return s.writer.emit("error", map[string]any{"code": code, "message": message, "param": nil})
	}
	s.sawChunk = true
	s.observeUsage(root["usage"])
	if err := s.observeSearch(root, nil, nil); err != nil {
		return err
	}
	for _, rawChoice := range anySlice(root["choices"]) {
		choice, _ := rawChoice.(map[string]any)
		if choice == nil || numberInt(choice["index"]) != 0 {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			delta = map[string]any{}
		}
		if text := firstString(delta, "reasoning_content", "reasoning"); text != "" {
			if err := s.appendReasoning(text); err != nil {
				return err
			}
		}
		if text := chatMessageText(delta["content"]); text != "" {
			if err := s.appendText(text); err != nil {
				return err
			}
		}
		for _, rawCall := range anySlice(delta["tool_calls"]) {
			call, _ := rawCall.(map[string]any)
			if call == nil {
				continue
			}
			if err := s.appendToolCall(call); err != nil {
				return err
			}
		}
		if err := s.observeSearch(root, choice, delta); err != nil {
			return err
		}
		if finish := stringValue(choice["finish_reason"]); finish != "" {
			s.finishReason = finish
		}
	}
	return nil
}

func (s *chatStreamState) observeUsage(value any) {
	usage, _ := value.(map[string]any)
	if len(usage) == 0 {
		return
	}
	var result canonicalResult
	applyChatUsage(&result, usage)
	if result.UsagePresent && result.LiveContextPresent {
		// Chat usage objects are cumulative snapshots. Last complete snapshot
		// wins; partial or placeholder objects cannot erase a prior measurement.
		s.usage = cloneMap(usage)
	}
}

func (s *chatStreamState) appendReasoning(text string) error {
	if s.reasoning == nil {
		item := reasoningItem("", "")
		item["status"] = "in_progress"
		s.reasoning = &messagesStreamBlock{kind: "thinking", item: item, outputIndex: s.appendItem(item)}
		if err := s.writer.emit("response.output_item.added", map[string]any{"output_index": s.reasoning.outputIndex, "item": cloneMap(item)}); err != nil {
			return err
		}
		if err := s.writer.emit("response.content_part.added", map[string]any{"output_index": s.reasoning.outputIndex, "item_id": stringValue(item["id"]), "content_index": 0, "part": map[string]any{"type": "reasoning_text", "text": ""}}); err != nil {
			return err
		}
	}
	s.reasoning.text.WriteString(text)
	return s.writer.emit("response.reasoning_text.delta", map[string]any{"output_index": s.reasoning.outputIndex, "item_id": stringValue(s.reasoning.item["id"]), "content_index": 0, "delta": text})
}

func (s *chatStreamState) appendText(text string) error {
	if s.message == nil {
		item := messageItem("", nil)
		item["status"] = "in_progress"
		s.message = &messagesStreamBlock{kind: "text", item: item, outputIndex: s.appendItem(item)}
		added := cloneMap(item)
		added["content"] = []any{}
		if err := s.writer.emit("response.output_item.added", map[string]any{"output_index": s.message.outputIndex, "item": added}); err != nil {
			return err
		}
		if err := s.writer.emit("response.content_part.added", map[string]any{"output_index": s.message.outputIndex, "item_id": stringValue(item["id"]), "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}}); err != nil {
			return err
		}
	}
	s.message.text.WriteString(text)
	return s.writer.emit("response.output_text.delta", map[string]any{"output_index": s.message.outputIndex, "item_id": stringValue(s.message.item["id"]), "content_index": 0, "delta": text, "logprobs": []any{}})
}

func (s *chatStreamState) appendToolCall(raw map[string]any) error {
	index := numberInt(raw["index"])
	call := s.tools[index]
	if call == nil {
		call = &chatToolStream{wireIndex: index, outputIndex: -1}
		s.tools[index] = call
	}
	if id := stringValue(raw["id"]); id != "" {
		call.callID = id
	}
	function, _ := raw["function"].(map[string]any)
	if name := stringValue(function["name"]); name != "" {
		call.name.WriteString(name)
	}
	arguments := stringValue(function["arguments"])
	if arguments != "" {
		call.arguments.WriteString(arguments)
	}
	if !call.started && call.callID != "" && call.name.Len() > 0 && call.arguments.Len() > 0 {
		if err := s.startToolCall(call); err != nil {
			return err
		}
		return s.writer.emit("response.function_call_arguments.delta", map[string]any{"output_index": call.outputIndex, "item_id": stringValue(call.item["id"]), "delta": call.arguments.String()})
	}
	if arguments != "" && call.started {
		return s.writer.emit("response.function_call_arguments.delta", map[string]any{"output_index": call.outputIndex, "item_id": stringValue(call.item["id"]), "delta": arguments})
	}
	return nil
}

func (s *chatStreamState) startToolCall(call *chatToolStream) error {
	name := restoreStreamToolName(call.name.String(), s.request.ClientSearchAlias)
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("Chat Completions tool call %d has no function name", call.wireIndex)
	}
	item := functionCallItem(call.callID, name, "")
	item["status"] = "in_progress"
	call.item, call.outputIndex, call.started = item, s.appendItem(item), true
	return s.writer.emit("response.output_item.added", map[string]any{"output_index": call.outputIndex, "item": cloneMap(item)})
}

func (s *chatStreamState) observeSearch(root, choice, delta map[string]any) error {
	urls := collectCitationURLs(root, choice, delta)
	s.allURLs = mergeUniqueStrings(s.allURLs, urls...)
	if !s.request.HostedWebSearch || (!chatSearchExecuted(root, choice, delta, urls) && s.search == nil) {
		return nil
	}
	if s.search == nil {
		item := webSearchItem("", s.request.SearchQuery, nil, "in_progress")
		s.search = &messagesStreamBlock{kind: "server_tool_use", item: item, outputIndex: s.appendItem(item)}
		if err := s.writer.emit("response.output_item.added", map[string]any{"output_index": s.search.outputIndex, "item": cloneMap(item)}); err != nil {
			return err
		}
		if err := s.writer.emit("response.web_search_call.in_progress", map[string]any{"output_index": s.search.outputIndex, "item_id": stringValue(item["id"])}); err != nil {
			return err
		}
		if err := s.writer.emit("response.web_search_call.searching", map[string]any{"output_index": s.search.outputIndex, "item_id": stringValue(item["id"])}); err != nil {
			return err
		}
	}
	mergeWebSearchSources(s.search.item, urlsToSources(s.allURLs))
	return nil
}

func (s *chatStreamState) finish() error {
	if s.terminal {
		return nil
	}
	if !s.sawChunk {
		return fmt.Errorf("Chat Completions stream contained no chunks")
	}
	searchConfirmed := s.search != nil || (s.request.HostedWebSearch && (len(s.allURLs) > 0 || positiveSearchUsage(s.usage)))
	if searchConfirmed && s.message != nil {
		s.allURLs = mergeUniqueStrings(s.allURLs, urlsFromText(s.message.text.String())...)
	}
	if s.reasoning != nil {
		text := s.reasoning.text.String()
		s.reasoning.item["status"] = "completed"
		if text != "" {
			s.reasoning.item["content"] = []any{map[string]any{"type": "reasoning_text", "text": text}}
		}
		if err := s.writer.emit("response.reasoning_text.done", map[string]any{"output_index": s.reasoning.outputIndex, "item_id": stringValue(s.reasoning.item["id"]), "content_index": 0, "text": text}); err != nil {
			return err
		}
		if err := s.writer.emit("response.content_part.done", map[string]any{"output_index": s.reasoning.outputIndex, "item_id": stringValue(s.reasoning.item["id"]), "content_index": 0, "part": map[string]any{"type": "reasoning_text", "text": text}}); err != nil {
			return err
		}
		if err := s.emitItemDone(s.reasoning); err != nil {
			return err
		}
	}
	if s.message != nil {
		text := s.message.text.String()
		annotations := urlsToAnnotations(s.allURLs)
		part := map[string]any{"type": "output_text", "text": text, "annotations": annotations, "logprobs": []any{}}
		s.message.item["status"] = "completed"
		s.message.item["content"] = []any{part}
		if err := s.writer.emit("response.output_text.done", map[string]any{"output_index": s.message.outputIndex, "item_id": stringValue(s.message.item["id"]), "content_index": 0, "text": text, "logprobs": []any{}}); err != nil {
			return err
		}
		if err := s.writer.emit("response.content_part.done", map[string]any{"output_index": s.message.outputIndex, "item_id": stringValue(s.message.item["id"]), "content_index": 0, "part": part}); err != nil {
			return err
		}
		if err := s.emitItemDone(s.message); err != nil {
			return err
		}
	}
	for _, index := range sortedToolIndexes(s.tools) {
		call := s.tools[index]
		if !call.started {
			if call.callID == "" {
				call.callID = compatID("call")
			}
			if err := s.startToolCall(call); err != nil {
				return err
			}
			if call.arguments.Len() > 0 {
				if err := s.writer.emit("response.function_call_arguments.delta", map[string]any{"output_index": call.outputIndex, "item_id": stringValue(call.item["id"]), "delta": call.arguments.String()}); err != nil {
					return err
				}
			}
		}
		arguments := call.arguments.String()
		if arguments == "" {
			arguments = "{}"
		}
		call.item["arguments"] = arguments
		call.item["status"] = "completed"
		if err := s.writer.emit("response.function_call_arguments.done", map[string]any{"output_index": call.outputIndex, "item_id": stringValue(call.item["id"]), "arguments": arguments}); err != nil {
			return err
		}
		if err := s.writer.emit("response.output_item.done", map[string]any{"output_index": call.outputIndex, "item": cloneMap(call.item)}); err != nil {
			return err
		}
	}
	if s.search == nil && s.request.HostedWebSearch && (len(s.allURLs) > 0 || positiveSearchUsage(s.usage)) {
		if err := s.observeSearch(map[string]any{"usage": s.usage, "citations": s.allURLs}, nil, nil); err != nil {
			return err
		}
	}
	if s.search != nil {
		mergeWebSearchSources(s.search.item, urlsToSources(s.allURLs))
		s.search.item["status"] = "completed"
		if err := s.writer.emit("response.web_search_call.completed", map[string]any{"output_index": s.search.outputIndex, "item_id": stringValue(s.search.item["id"])}); err != nil {
			return err
		}
		if err := s.writer.emit("response.output_item.done", map[string]any{"output_index": s.search.outputIndex, "item": cloneMap(s.search.item)}); err != nil {
			return err
		}
	}
	result := canonicalResult{Output: s.output}
	applyChatUsage(&result, s.usage)
	applyChatFinishReason(&result, s.finishReason)
	terminal := translatedResponse(s.route, s.request, result, s.responseID, s.createdAt)
	if err := validateResponsesEnvelope(terminal); err != nil {
		return err
	}
	typ := "response.completed"
	if result.FailureCode != "" {
		typ = "response.failed"
	} else if result.IncompleteReason != "" {
		typ = "response.incomplete"
	}
	s.terminal = true
	return s.writer.emit(typ, map[string]any{"response": terminal})
}

func (s *chatStreamState) appendItem(item map[string]any) int {
	index := len(s.output)
	s.output = append(s.output, item)
	return index
}

func (s *chatStreamState) emitItemDone(block *messagesStreamBlock) error {
	return s.writer.emit("response.output_item.done", map[string]any{"output_index": block.outputIndex, "item": cloneMap(block.item)})
}

func (s *Server) streamChatSSE(w http.ResponseWriter, response *http.Response, route config.Route, request facadeRequest, started time.Time) {
	writer, err := beginTranslatedStream(w, response)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	modelObserver := newUpstreamModelObserver(request.Protocol)
	defer modelObserver.log(s.log, route)
	state := newChatStreamState(writer, route, request)
	if err := writer.emitStart(route, request, state.responseID, state.createdAt); err != nil {
		return
	}
	evidence := newSearchEvidence()
	heartbeats := 0
	streamErr := scanSSEPayloads(response.Body, func(lines []string, payload []byte) error {
		if isPrivateSSEHeartbeat(lines, string(payload)) {
			heartbeats++
			return writer.emitHeartbeat()
		}
		modelObserver.observeJSON(payload, false)
		if strings.TrimSpace(string(payload)) != "[DONE]" {
			evidence.observeJSON(payload)
		}
		if err := state.handle(payload); err != nil {
			return err
		}
		s.captureReasoningProvenance(route, state.output)
		if state.terminal {
			return errSSEStreamComplete
		}
		return nil
	})
	if errors.Is(streamErr, errSSEStreamComplete) {
		streamErr = nil
	}
	if streamErr == nil && !state.terminal && state.finishReason != "" {
		streamErr = state.finish()
	}
	if streamErr != nil {
		s.log.Printf("UP channel=%s Chat Completions SSE conversion error: %v", route.ChannelID, streamErr)
		writer.emitStreamError(upstreamStreamFailureMessage("Chat Completions", streamErr))
	} else if !state.terminal {
		s.log.Printf("UP channel=%s Chat Completions SSE ended without [DONE] or finish_reason", route.ChannelID)
		writer.emitStreamError("upstream Chat Completions stream ended without a terminal chunk")
	}
	s.log.Printf("UP channel=%s Chat Completions SSE done events=%d heartbeats=%d terminal=%t %s", route.ChannelID, writer.sequence, heartbeats, state.terminal, time.Since(started).Round(time.Millisecond))
	s.logSearchEvidence(route.ChannelID, request, evidence)
}

func (s *Server) streamNativeSSE(w http.ResponseWriter, response *http.Response, route config.Route, request facadeRequest, started time.Time) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "stream unsupported")
		return
	}
	modelObserver := newUpstreamModelObserver(request.Protocol)
	defer modelObserver.log(s.log, route)
	copySafeResponseHeaders(w.Header(), response.Header)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(response.StatusCode)
	flusher.Flush()

	terminal := false
	heartbeats := 0
	frames := 0
	chatStreamID := compatID("chatcmpl")
	chatCreatedAt := time.Now().Unix()
	messagesSearchFilter := newMessagesHostedSearchStreamFilter(request)
	evidence := newSearchEvidence()
	writeHeartbeat := func() error {
		_, err := io.WriteString(w, ": keepalive\n\n")
		if err == nil {
			flusher.Flush()
		}
		return err
	}
	streamErr := scanSSEPayloads(response.Body, func(lines []string, payload []byte) error {
		if isPrivateSSEHeartbeat(lines, string(payload)) {
			heartbeats++
			return writeHeartbeat()
		}
		trimmed := strings.TrimSpace(string(payload))
		if request.Protocol == wireChatCompletions && trimmed == "[DONE]" {
			if err := writeSSEPayloadFrame(w, flusher, lines, []byte("[DONE]")); err != nil {
				return err
			}
			frames++
			terminal = true
			return errSSEStreamComplete
		}
		root, err := decodeJSONMap(payload)
		if err != nil {
			return fmt.Errorf("decode upstream %s SSE frame: %w", protocolLabel(request.Protocol), err)
		}
		modelObserver.observe(root, false)
		evidence.observeJSON(payload)
		if deepSeekChatInsufficientSystemResource(root, route, request.Protocol) {
			writeNativeChatStreamError(
				w,
				flusher,
				"server_error",
				"insufficient_system_resource",
				errDeepSeekInsufficientSystemResource.Error(),
			)
			terminal = true
			return errSSEStreamComplete
		}
		if messagesSearchFilter != nil && !messagesSearchFilter.keep(root) {
			return nil
		}
		if request.Protocol == wireMessages {
			normalizeMessagesStreamRequiredFields(root)
		}
		if request.Protocol == wireChatCompletions {
			normalizeNativeChatRequiredFields(root, route, true, chatStreamID, chatCreatedAt)
			normalizeNativeChatUsage(root)
		}
		setDownstreamResponseModel(root, responseModelForRoute(route))
		restoreClientWebSearchAlias(root, request.ClientSearchAlias, request.Protocol)
		s.captureReasoningProvenance(route, root)
		encoded, err := json.Marshal(root)
		if err != nil {
			return err
		}
		if err := validateNativeSSEFrame(request.Protocol, root); err != nil {
			return err
		}
		if err := writeSSEPayloadFrame(w, flusher, lines, encoded); err != nil {
			return err
		}
		frames++
		if request.Protocol == wireMessages {
			typ := stringValue(root["type"])
			if typ == "message_stop" || typ == "error" {
				terminal = true
				return errSSEStreamComplete
			}
		}
		if request.Protocol == wireChatCompletions && root["error"] != nil {
			terminal = true
			return errSSEStreamComplete
		}
		return nil
	})
	if errors.Is(streamErr, errSSEStreamComplete) {
		streamErr = nil
	}
	if streamErr != nil {
		s.log.Printf("UP channel=%s %s SSE read error: %v", route.ChannelID, protocolLabel(request.Protocol), streamErr)
		writeNativeStreamError(w, flusher, request.Protocol, upstreamStreamFailureMessage(protocolLabel(request.Protocol), streamErr))
	} else if !terminal {
		s.log.Printf("UP channel=%s %s SSE ended without a terminal event", route.ChannelID, protocolLabel(request.Protocol))
		writeNativeStreamError(w, flusher, request.Protocol, "upstream "+protocolLabel(request.Protocol)+" stream ended without a terminal event")
	}
	s.log.Printf("UP channel=%s %s SSE done frames=%d heartbeats=%d terminal=%t %s",
		route.ChannelID, protocolLabel(request.Protocol), frames, heartbeats, terminal, time.Since(started).Round(time.Millisecond))
	s.logSearchEvidence(route.ChannelID, request, evidence)
}

type messagesHostedSearchStreamFilter struct {
	dropped map[int]struct{}
}

func newMessagesHostedSearchStreamFilter(request facadeRequest) *messagesHostedSearchStreamFilter {
	if request.Protocol != wireMessages || !request.HostedWebSearch {
		return nil
	}
	return &messagesHostedSearchStreamFilter{dropped: make(map[int]struct{})}
}

func (filter *messagesHostedSearchStreamFilter) keep(root map[string]any) bool {
	switch stringValue(root["type"]) {
	case "message_start":
		if message, _ := root["message"].(map[string]any); message != nil {
			stripMessagesHostedSearchBlocks(message)
		}
	case "content_block_start":
		index := numberInt(root["index"])
		block, _ := root["content_block"].(map[string]any)
		switch stringValue(block["type"]) {
		case "server_tool_use", "web_search_tool_result":
			filter.dropped[index] = struct{}{}
			return false
		}
	case "content_block_delta":
		index := numberInt(root["index"])
		if _, dropped := filter.dropped[index]; dropped {
			return false
		}
		if delta, _ := root["delta"].(map[string]any); stringValue(delta["type"]) == "citations_delta" {
			return false
		}
	case "content_block_stop":
		index := numberInt(root["index"])
		if _, dropped := filter.dropped[index]; dropped {
			delete(filter.dropped, index)
			return false
		}
	}
	return true
}

func validateNativeSSEFrame(protocol wireProtocol, root map[string]any) error {
	if protocol == wireChatCompletions && root["error"] != nil {
		return nil
	}
	switch protocol {
	case wireMessages:
		typ := stringValue(root["type"])
		if strings.TrimSpace(typ) == "" {
			return fmt.Errorf("Messages SSE event type must be a non-empty string")
		}
		switch typ {
		case "message_start":
			message, ok := root["message"].(map[string]any)
			if !ok || message == nil {
				return fmt.Errorf("Messages message_start has no message object")
			}
			return validateNativeMessagesEnvelope(message)
		case "message_delta":
			delta, ok := root["delta"].(map[string]any)
			if !ok || delta == nil {
				return fmt.Errorf("Messages message_delta has no delta object")
			}
			if err := validateMessagesDeltaBody(delta); err != nil {
				return err
			}
			usage, ok := root["usage"].(map[string]any)
			if !ok || usage == nil {
				return fmt.Errorf("Messages message_delta has no usage object")
			}
			return validateMessagesUsage(usage, false)
		case "content_block_start":
			if err := validateRequiredU32(root, "index"); err != nil {
				return fmt.Errorf("Messages content_block_start: %w", err)
			}
			block, ok := root["content_block"].(map[string]any)
			if !ok || block == nil {
				return fmt.Errorf("Messages content_block_start has no content_block object")
			}
			return validateNativeMessagesContentBlock(block)
		case "content_block_delta":
			if err := validateRequiredU32(root, "index"); err != nil {
				return fmt.Errorf("Messages content_block_delta: %w", err)
			}
			delta, ok := root["delta"].(map[string]any)
			if !ok || delta == nil {
				return fmt.Errorf("Messages content_block_delta has no delta object")
			}
			return validateNativeMessagesStreamDelta(delta)
		case "content_block_stop":
			if err := validateRequiredU32(root, "index"); err != nil {
				return fmt.Errorf("Messages content_block_stop: %w", err)
			}
			return nil
		case "message_stop", "ping":
			return nil
		case "error":
			errorBody, ok := root["error"].(map[string]any)
			if !ok || errorBody == nil {
				return fmt.Errorf("Messages error event has no error object")
			}
			if _, ok := errorBody["type"].(string); !ok {
				return fmt.Errorf("Messages error type must be a string")
			}
			if _, ok := errorBody["message"].(string); !ok {
				return fmt.Errorf("Messages error message must be a string")
			}
			return nil
		default:
			return nil
		}
	case wireChatCompletions:
		return validateNativeChatChunk(root)
	default:
		return fmt.Errorf("unsupported native stream protocol")
	}
}

func writeSSEPayloadFrame(w io.Writer, flusher http.Flusher, lines []string, payload []byte) error {
	wroteData := false
	for _, line := range lines {
		if strings.HasPrefix(line, "data:") {
			if !wroteData {
				if _, err := fmt.Fprintf(w, "data: %s\n", payload); err != nil {
					return err
				}
				wroteData = true
			}
			continue
		}
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return err
		}
	}
	if !wroteData {
		if _, err := fmt.Fprintf(w, "data: %s\n", payload); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeNativeStreamError(w io.Writer, flusher http.Flusher, protocol wireProtocol, message string) {
	switch protocol {
	case wireMessages:
		payload, _ := json.Marshal(map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "proxy_stream_error", "message": message},
		})
		_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
	case wireChatCompletions:
		writeNativeChatStreamError(w, flusher, "proxy_stream_error", "proxy_stream_error", message)
		return
	}
	flusher.Flush()
}

func writeNativeChatStreamError(w io.Writer, flusher http.Flusher, errorType, code, message string) {
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{"type": errorType, "code": code, "message": message},
	})
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
	flusher.Flush()
}

func writeMessagesSSEFallback(w http.ResponseWriter, response map[string]any) error {
	if err := validateNativeMessagesEnvelope(response); err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	emit := func(typ string, value map[string]any) error {
		value["type"] = typ
		payload, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", typ, payload)
		if flusher != nil {
			flusher.Flush()
		}
		return err
	}
	started := cloneMap(response)
	started["content"] = []any{}
	started["stop_reason"] = nil
	started["stop_sequence"] = nil
	if usage, _ := started["usage"].(map[string]any); usage != nil {
		usage["output_tokens"] = 0
	}
	if err := emit("message_start", map[string]any{"message": started}); err != nil {
		return err
	}
	for index, raw := range anySlice(response["content"]) {
		block, _ := raw.(map[string]any)
		initial, deltas := messagesFallbackBlock(block)
		if err := emit("content_block_start", map[string]any{"index": index, "content_block": initial}); err != nil {
			return err
		}
		for _, delta := range deltas {
			if err := emit("content_block_delta", map[string]any{"index": index, "delta": delta}); err != nil {
				return err
			}
		}
		if err := emit("content_block_stop", map[string]any{"index": index}); err != nil {
			return err
		}
	}
	delta := map[string]any{"stop_reason": response["stop_reason"], "stop_sequence": response["stop_sequence"]}
	usage := map[string]any{}
	if original, _ := response["usage"].(map[string]any); original != nil {
		if output, exists := original["output_tokens"]; exists {
			usage["output_tokens"] = output
		}
	}
	if err := emit("message_delta", map[string]any{"delta": delta, "usage": usage}); err != nil {
		return err
	}
	return emit("message_stop", map[string]any{})
}

func messagesFallbackBlock(block map[string]any) (map[string]any, []map[string]any) {
	initial := cloneMap(block)
	var deltas []map[string]any
	switch stringValue(block["type"]) {
	case "text":
		initial["text"] = ""
		deltas = append(deltas, map[string]any{"type": "text_delta", "text": stringValue(block["text"])})
	case "thinking":
		initial["thinking"] = ""
		initial["signature"] = ""
		if text := stringValue(block["thinking"]); text != "" {
			deltas = append(deltas, map[string]any{"type": "thinking_delta", "thinking": text})
		}
		if signature := stringValue(block["signature"]); signature != "" {
			deltas = append(deltas, map[string]any{"type": "signature_delta", "signature": signature})
		}
	case "tool_use", "server_tool_use":
		input := block["input"]
		initial["input"] = map[string]any{}
		encoded, _ := json.Marshal(valueOr(input, map[string]any{}))
		deltas = append(deltas, map[string]any{"type": "input_json_delta", "partial_json": string(encoded)})
	}
	return initial, deltas
}

func writeChatSSEFallback(w http.ResponseWriter, response map[string]any) error {
	if err := validateNativeChatEnvelope(response); err != nil {
		return err
	}
	chunk := cloneMap(response)
	chunk["object"] = "chat.completion.chunk"
	choices := anySlice(chunk["choices"])
	for _, raw := range choices {
		choice, _ := raw.(map[string]any)
		choice["delta"] = valueOr(choice["message"], map[string]any{})
		delete(choice, "message")
	}
	payload, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, err = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", payload)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return err
}

func restoreStreamToolName(name, alias string) string {
	if alias != "" && strings.EqualFold(strings.TrimSpace(name), strings.TrimSpace(alias)) {
		return "web_search"
	}
	return name
}

func urlsFromJSON(value any) []string {
	seen := map[string]bool{}
	var urls []string
	var walk func(any, bool)
	walk = func(current any, urlContext bool) {
		switch typed := current.(type) {
		case map[string]any:
			for key, child := range typed {
				key = strings.ToLower(key)
				nextContext := urlContext || key == "url" || key == "uri" || isSearchSourceContainer(key)
				walk(child, nextContext)
			}
		case []any:
			for _, child := range typed {
				walk(child, urlContext)
			}
		case string:
			value := strings.TrimSpace(typed)
			if urlContext && (strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://")) && !seen[value] {
				seen[value] = true
				urls = append(urls, value)
			}
		}
	}
	walk(value, false)
	return urls
}

func mergeUniqueStrings(existing []string, values ...string) []string {
	seen := make(map[string]bool, len(existing)+len(values))
	for _, value := range existing {
		seen[value] = true
	}
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			existing = append(existing, value)
		}
	}
	return existing
}

func mergeAnnotations(existing, values []any) []any {
	seen := map[string]bool{}
	for _, raw := range existing {
		annotation, _ := raw.(map[string]any)
		seen[stringValue(annotation["url"])] = true
	}
	for _, raw := range values {
		annotation, _ := raw.(map[string]any)
		url := stringValue(annotation["url"])
		if url != "" && !seen[url] {
			seen[url] = true
			existing = append(existing, raw)
		}
	}
	return existing
}

func mergeWebSearchSources(item map[string]any, values []any) {
	action, _ := item["action"].(map[string]any)
	if action == nil {
		return
	}
	existing := anySlice(action["sources"])
	seen := map[string]bool{}
	for _, raw := range existing {
		source, _ := raw.(map[string]any)
		seen[stringValue(source["url"])] = true
	}
	for _, raw := range values {
		source, _ := raw.(map[string]any)
		url := stringValue(source["url"])
		if url != "" && !seen[url] {
			seen[url] = true
			existing = append(existing, raw)
		}
	}
	action["sources"] = existing
}

func anySliceOrValue(value any) []any {
	if values, ok := value.([]any); ok {
		return values
	}
	if value == nil {
		return nil
	}
	return []any{value}
}

func sortedBlockIndexes(blocks map[int]*messagesStreamBlock) []int {
	indexes := make([]int, 0, len(blocks))
	for index := range blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	return indexes
}

func sortedToolIndexes(tools map[int]*chatToolStream) []int {
	indexes := make([]int, 0, len(tools))
	for index := range tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	return indexes
}

func nestedNumber(root map[string]any, parent, child string) int {
	values, _ := root[parent].(map[string]any)
	return numberInt(values[child])
}
