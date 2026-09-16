package proxy

import (
	"sort"
	"strings"
)

// streamFrameRectifier is the shared contract for stateful, frame-rewriting
// stream rectifiers (chatToolRectifier, messagesToolRectifier,
// responsesToolRectifier). Each consumes one decoded SSE frame and returns
// the replacement frames to forward; a rectifier that holds the frame for
// later reassembly returns nil. Notes surface relay defects in the proxy log.
type streamFrameRectifier interface {
	ingest(root map[string]any) ([]map[string]any, []string)
}

// rectifierNotes adapts a rectifier whose ingest does not report notes to the
// shared contract.
type rectifierNotes struct {
	ingestFunc func(root map[string]any) []map[string]any
}

func (r rectifierNotes) ingest(root map[string]any) ([]map[string]any, []string) {
	return r.ingestFunc(root), nil
}

// messagesToolRectifier holds Messages tool_use blocks between
// content_block_start and content_block_stop. The function name only exists
// on the start frame, so a relay that drops it leaves no recovery point in
// later frames; holding the block lets the accumulated input JSON resolve the
// name before anything reaches Grok Build. The block is re-emitted as start
// (resolved name) + one complete input_json_delta + the original stop.
type messagesToolRectifier struct {
	advertised []advertisedTool
	blocks     map[int]*messagesToolBlock
}

type messagesToolBlock struct {
	index int
	name  string
	args  strings.Builder
	start map[string]any
}

func newMessagesToolRectifier(advertised []advertisedTool) *messagesToolRectifier {
	return &messagesToolRectifier{advertised: advertised, blocks: map[int]*messagesToolBlock{}}
}

func (r *messagesToolRectifier) ingest(root map[string]any) []map[string]any {
	if r == nil || root == nil {
		return []map[string]any{root}
	}
	switch stringValue(root["type"]) {
	case "content_block_start":
		block, _ := root["content_block"].(map[string]any)
		if block == nil || stringValue(block["type"]) != "tool_use" {
			break
		}
		index := numberInt(root["index"])
		r.blocks[index] = &messagesToolBlock{index: index, name: stringValue(block["name"]), start: cloneMap(root)}
		return nil
	case "content_block_delta":
		held := r.blocks[numberInt(root["index"])]
		if held == nil {
			break
		}
		delta, _ := root["delta"].(map[string]any)
		if delta != nil && stringValue(delta["type"]) == "input_json_delta" {
			held.args.WriteString(stringValue(delta["partial_json"]))
			return nil
		}
	case "content_block_stop":
		index := numberInt(root["index"])
		held := r.blocks[index]
		if held == nil {
			break
		}
		delete(r.blocks, index)
		return append(r.emit(held), root)
	case "message_stop", "error":
		if len(r.blocks) > 0 {
			out := r.flushHeld()
			return append(out, root)
		}
	}
	return []map[string]any{root}
}

func (r *messagesToolRectifier) flushHeld() []map[string]any {
	indexes := make([]int, 0, len(r.blocks))
	for index := range r.blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	var out []map[string]any
	for _, index := range indexes {
		held := r.blocks[index]
		delete(r.blocks, index)
		out = append(out, r.emit(held)...)
	}
	return out
}

func (r *messagesToolRectifier) emit(held *messagesToolBlock) []map[string]any {
	name := held.name
	args := held.args.String()
	resolved, rewritten, _ := adaptResolvedCall(name, args, r.advertised)
	if strings.TrimSpace(resolved) != "" {
		name = resolved
	}
	if rewritten != "" {
		args = rewritten
	}
	start := held.start
	if block, _ := start["content_block"].(map[string]any); block != nil {
		block["name"] = name
	}
	out := []map[string]any{start}
	if strings.TrimSpace(args) != "" {
		out = append(out, map[string]any{
			"type":  "content_block_delta",
			"index": held.index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})
	}
	return out
}

// responsesToolRectifier holds Responses function_call items between
// response.output_item.added and response.output_item.done for the same
// reason: the added frame is the only stream-level carrier of the name, while
// the done frame repeats the full item and therefore offers a second source
// for both name and arguments when the relay dropped or truncated either.
type responsesToolRectifier struct {
	advertised []advertisedTool
	items      map[int]*responsesToolItem
}

type responsesToolItem struct {
	outputIndex int
	name        string
	args        strings.Builder
	added       map[string]any
}

func newResponsesToolRectifier(advertised []advertisedTool) *responsesToolRectifier {
	return &responsesToolRectifier{advertised: advertised, items: map[int]*responsesToolItem{}}
}

func (r *responsesToolRectifier) ingest(root map[string]any) []map[string]any {
	if r == nil || root == nil {
		return []map[string]any{root}
	}
	switch stringValue(root["type"]) {
	case "response.output_item.added":
		item, _ := root["item"].(map[string]any)
		if item == nil || stringValue(item["type"]) != "function_call" {
			break
		}
		index := numberInt(root["output_index"])
		r.items[index] = &responsesToolItem{outputIndex: index, name: stringValue(item["name"]), added: cloneMap(root)}
		return nil
	case "response.function_call_arguments.delta":
		if held := r.items[numberInt(root["output_index"])]; held != nil {
			held.args.WriteString(stringValue(root["delta"]))
			return nil
		}
	case "response.output_item.done":
		index := numberInt(root["output_index"])
		held := r.items[index]
		if held == nil {
			break
		}
		delete(r.items, index)
		if item, _ := root["item"].(map[string]any); item != nil {
			if name := stringValue(item["name"]); name != "" {
				held.name = name
			}
			if args := stringValue(item["arguments"]); jsonObjectComplete(args) && !jsonObjectComplete(held.args.String()) {
				held.args.Reset()
				held.args.WriteString(args)
			}
			item["name"], item["arguments"] = r.resolve(held)
		}
		return append(r.emit(held), root)
	case "response.completed", "response.incomplete", "response.failed":
		if len(r.items) > 0 {
			out := r.flushHeld()
			return append(out, root)
		}
	}
	return []map[string]any{root}
}

func (r *responsesToolRectifier) flushHeld() []map[string]any {
	indexes := make([]int, 0, len(r.items))
	for index := range r.items {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	var out []map[string]any
	for _, index := range indexes {
		held := r.items[index]
		delete(r.items, index)
		out = append(out, r.emit(held)...)
	}
	return out
}

func (r *responsesToolRectifier) resolve(held *responsesToolItem) (string, string) {
	name := held.name
	args := held.args.String()
	resolved, rewritten, _ := adaptResolvedCall(name, args, r.advertised)
	if strings.TrimSpace(resolved) != "" {
		name = resolved
	}
	if rewritten != "" {
		args = rewritten
	}
	return name, args
}

func (r *responsesToolRectifier) emit(held *responsesToolItem) []map[string]any {
	name, args := r.resolve(held)
	added := held.added
	if item, _ := added["item"].(map[string]any); item != nil {
		item["name"] = name
	}
	out := []map[string]any{added}
	if strings.TrimSpace(args) != "" {
		out = append(out, map[string]any{
			"type":         "response.function_call_arguments.delta",
			"output_index": held.outputIndex,
			"item_id":      r.itemID(held),
			"delta":        args,
		})
	}
	return out
}

func (r *responsesToolRectifier) itemID(held *responsesToolItem) string {
	if item, _ := held.added["item"].(map[string]any); item != nil {
		return stringValue(item["id"])
	}
	return ""
}
