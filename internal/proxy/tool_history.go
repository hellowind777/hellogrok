package proxy

import "fmt"

func normalizeChatToolHistory(value any) error {
	messages, ok := value.([]any)
	if !ok {
		return nil
	}
	for mi, raw := range messages {
		m, _ := raw.(map[string]any)
		if m == nil || stringValue(m["role"]) != "assistant" {
			continue
		}
		var missing []map[string]any
		known := map[string]bool{}
		for _, rawCall := range anySlice(m["tool_calls"]) {
			call, _ := rawCall.(map[string]any)
			if call == nil {
				continue
			}
			id := stringValue(call["id"])
			if id == "" {
				missing = append(missing, call)
			} else {
				known[id] = true
			}
		}
		if len(missing) == 0 {
			continue
		}
		var candidates []string
		for _, rawResult := range messages[mi+1:] {
			result, _ := rawResult.(map[string]any)
			if stringValue(result["role"]) != "tool" {
				break
			}
			id := stringValue(result["tool_call_id"])
			if id != "" && !known[id] {
				candidates = append(candidates, id)
			}
		}
		if len(missing) != 1 || len(candidates) != 1 {
			return fmt.Errorf("Chat Completions tool history is invalid: missing tool call IDs cannot be associated unambiguously; start a new session")
		}
		missing[0]["id"] = candidates[0]
	}
	return nil
}

// IDs are fixed before delivery and retained across deltas, including late IDs.
type chatCallIDs map[string]string

func (ids chatCallIDs) normalize(root map[string]any, stream bool) error {
	for _, rawChoice := range anySlice(root["choices"]) {
		choice, _ := rawChoice.(map[string]any)
		field := "message"
		if stream {
			field = "delta"
		}
		message, _ := choice[field].(map[string]any)
		for position, rawCall := range anySlice(message["tool_calls"]) {
			call, _ := rawCall.(map[string]any)
			if call == nil {
				return fmt.Errorf("tool call must be an object")
			}
			index := position
			if stream {
				if _, present, valid := optionalCanonicalToken(call, "index"); !present || !valid {
					return fmt.Errorf("stream tool call requires an explicit nonnegative index")
				}
				index = numberInt(call["index"])
			}
			key := fmt.Sprintf("%d:%d", numberInt(choice["index"]), index)
			id := ids[key]
			if id == "" {
				id = stringValue(call["id"])
				if id == "" {
					id = compatID("call")
				}
				for other, used := range ids {
					if other != key && used == id {
						return fmt.Errorf("duplicate tool call ID")
					}
				}
				ids[key] = id
			}
			call["id"] = id
		}
	}
	return nil
}
