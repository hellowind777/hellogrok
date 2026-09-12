package proxy

import (
	"fmt"
	"strings"
)

type responseItemIDs struct {
	id     string
	callID string
}

// One registry belongs to one response, not to a connection or channel.
type responseIDs struct {
	items map[int]*responseItemIDs
	// remapped counts upstream items whose id collided with another output
	// slot and was rewritten to keep the stream alive.
	remapped int
}

func (ids *responseIDs) item(index int, item map[string]any) error {
	if ids.items == nil {
		ids.items = map[int]*responseItemIDs{}
	}
	entry := ids.items[index]
	if entry == nil {
		entry = &responseItemIDs{}
		ids.items[index] = entry
	}
	if err := ids.assign(index, &entry.id, item, "id", "item"); err != nil {
		return err
	}
	if stringValue(item["type"]) == "function_call" {
		return ids.assign(index, &entry.callID, item, "call_id", "call")
	}
	return nil
}

func (ids *responseIDs) assign(index int, saved *string, object map[string]any, key, prefix string) error {
	value := stringValue(object[key])
	if *saved != "" && value != "" && value != *saved {
		if !ids.collides(index, value) {
			return fmt.Errorf("Responses output %d has conflicting %s", index, key)
		}
		// The value belongs to a different slot whose id this provider
		// reused; treat the event as referring to this slot and rewrite
		// it below.
	}
	if *saved == "" {
		if value == "" {
			value = compatID(prefix)
		}
		if ids.collides(index, value) {
			// Some providers reuse one item id across output slots (for
			// example a single reasoning id per response). Remap the later
			// slot to a fresh id instead of failing the whole stream.
			value = ids.freshID(value, prefix)
			ids.remapped++
		}
		*saved = value
	}
	object[key] = *saved
	return nil
}

func (ids *responseIDs) collides(self int, value string) bool {
	for other, entry := range ids.items {
		if other != self && (entry.id == value || entry.callID == value) {
			return true
		}
	}
	return false
}

// freshID keeps the provider's type prefix (rs_, ws_, msg_, ...) so the
// rewritten id still reads like the item it labels.
func (ids *responseIDs) freshID(original, prefix string) string {
	base := prefix
	if cut := strings.IndexByte(original, '_'); cut > 0 {
		base = original[:cut]
	}
	for {
		candidate := compatID(base)
		if !ids.collides(-1, candidate) {
			return candidate
		}
	}
}

func (ids *responseIDs) normalize(event map[string]any) error {
	if response, _ := event["response"].(map[string]any); response != nil {
		for index, raw := range anySlice(response["output"]) {
			if item, _ := raw.(map[string]any); item != nil {
				if err := ids.item(index, item); err != nil {
					return err
				}
			}
		}
	}
	item, _ := event["item"].(map[string]any)
	typ := stringValue(event["type"])
	_, hasItemID := event["item_id"]
	if item == nil && !hasItemID && !strings.HasPrefix(typ, "response.function_call_arguments.") {
		return nil
	}
	if _, present, valid := optionalCanonicalToken(event, "output_index"); !present || !valid {
		return fmt.Errorf("Responses item event requires output_index")
	}
	index := numberInt(event["output_index"])
	if item != nil {
		return ids.item(index, item)
	}
	if ids.items == nil {
		ids.items = map[int]*responseItemIDs{}
	}
	entry := ids.items[index]
	if entry == nil {
		entry = &responseItemIDs{}
		ids.items[index] = entry
	}
	alias := map[string]any{"id": event["item_id"]}
	if err := ids.assign(index, &entry.id, alias, "id", "item"); err != nil {
		return err
	}
	event["item_id"] = entry.id
	return nil
}

func normalizeMessagesToolIDs(root map[string]any) {
	for _, raw := range anySlice(root["content"]) {
		block, _ := raw.(map[string]any)
		if stringValue(block["type"]) == "tool_use" && stringValue(block["id"]) == "" {
			block["id"] = compatID("toolu")
		}
	}
}
