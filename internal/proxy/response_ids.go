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
type responseIDs map[int]*responseItemIDs

func (ids responseIDs) item(index int, item map[string]any) error {
	entry := ids[index]
	if entry == nil {
		entry = &responseItemIDs{}
		ids[index] = entry
	}
	if err := ids.assign(index, &entry.id, item, "id", "item"); err != nil {
		return err
	}
	if stringValue(item["type"]) == "function_call" {
		return ids.assign(index, &entry.callID, item, "call_id", "call")
	}
	return nil
}

func (ids responseIDs) assign(index int, saved *string, object map[string]any, key, prefix string) error {
	value := stringValue(object[key])
	if *saved != "" && value != "" && value != *saved {
		return fmt.Errorf("Responses output %d has conflicting %s", index, key)
	}
	if *saved == "" {
		if value == "" {
			value = compatID(prefix)
		}
		for other, entry := range ids {
			if other != index && ((key == "id" && entry.id == value) || (key == "call_id" && entry.callID == value)) {
				return fmt.Errorf("Responses output has duplicate %s", key)
			}
		}
		*saved = value
	}
	object[key] = *saved
	return nil
}

func (ids responseIDs) normalize(event map[string]any) error {
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
	if item == nil && !strings.HasPrefix(typ, "response.function_call_arguments.") {
		return nil
	}
	if _, present, valid := optionalCanonicalToken(event, "output_index"); !present || !valid {
		return fmt.Errorf("Responses item event requires output_index")
	}
	index := numberInt(event["output_index"])
	if item != nil {
		return ids.item(index, item)
	}
	entry := ids[index]
	if entry == nil {
		entry = &responseItemIDs{}
		ids[index] = entry
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
