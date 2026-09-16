package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
)

// visionDeniedPlaceholder replaces image content parts once a channel proved
// it cannot accept image input. The conversation keeps its turn structure and
// the model sees why the visual payload is missing.
const visionDeniedPlaceholder = "[hellogrok: image content omitted; this channel's model rejected image input (vision_not_supported)]"

// imagePartTypes are the content-part type tags that carry visual payload on
// the supported wires: chat completions (image_url), responses (input_image),
// and messages (image).
var imagePartTypes = map[string]struct{}{
	"image_url":   {},
	"input_image": {},
	"image":       {},
}

func isImagePart(part map[string]any) bool {
	_, ok := imagePartTypes[strings.ToLower(stringValue(part["type"]))]
	return ok
}

func visionPlaceholderPart(protocol wireProtocol) map[string]any {
	if protocol == wireResponses {
		return map[string]any{"type": "input_text", "text": visionDeniedPlaceholder}
	}
	return map[string]any{"type": "text", "text": visionDeniedPlaceholder}
}

// stripVisionContent replaces every image content part in the request body
// with a text placeholder and reports how many parts were removed. Bodies
// without image parts are returned unchanged.
func stripVisionContent(body []byte, protocol wireProtocol) ([]byte, int) {
	root, err := decodeJSONMap(body)
	if err != nil || root == nil {
		return body, 0
	}
	removed := stripVisionValue(root, protocol)
	if removed == 0 {
		return body, 0
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return body, 0
	}
	return encoded, removed
}

func stripVisionValue(value any, protocol wireProtocol) int {
	removed := 0
	switch typed := value.(type) {
	case map[string]any:
		for _, child := range typed {
			removed += stripVisionValue(child, protocol)
		}
	case []any:
		for i, item := range typed {
			part, ok := item.(map[string]any)
			if ok && isImagePart(part) {
				typed[i] = visionPlaceholderPart(protocol)
				removed++
				continue
			}
			removed += stripVisionValue(item, protocol)
		}
	}
	return removed
}

// isVisionUnsupportedError reports whether the upstream rejected the request
// because its model cannot accept image input.
func isVisionUnsupportedError(status int, data []byte) bool {
	if status != http.StatusBadRequest {
		return false
	}
	summary := summarizeUpstreamError(data)
	code := strings.ToLower(summary.Code)
	message := strings.ToLower(summary.Message)
	return strings.Contains(code, "vision") ||
		strings.Contains(message, "not multimodal") ||
		strings.Contains(message, "does not support image")
}
