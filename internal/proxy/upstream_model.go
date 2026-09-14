package proxy

import (
	"log"
	"net"
	"strings"
	"sync"

	"github.com/hellowind777/hellogrok/internal/config"
)

const maxResponseModelSilenceKeys = 256

var (
	responseModelSilenceMu sync.Mutex
	responseModelSilence   = map[string]struct{}{}
)

type upstreamModelObserver struct {
	protocol     wireProtocol
	first        string
	terminal     string
	declarations int
	invalid      int
	conflict     bool
}

func newUpstreamModelObserver(protocol wireProtocol) *upstreamModelObserver {
	return &upstreamModelObserver{protocol: protocol}
}

func (observer *upstreamModelObserver) observeJSON(data []byte, forceTerminal bool) {
	root, err := decodeJSONMap(data)
	if err != nil {
		return
	}
	observer.observe(root, forceTerminal)
}

func (observer *upstreamModelObserver) observe(root map[string]any, forceTerminal bool) {
	raw, exists := declaredUpstreamModel(observer.protocol, root)
	if !exists {
		return
	}
	model, ok := validObservedUpstreamModel(raw)
	if !ok {
		observer.invalid++
		return
	}
	observer.declarations++
	if observer.first == "" {
		observer.first = model
	} else if !strings.EqualFold(observer.first, model) {
		observer.conflict = true
	}
	if forceTerminal || isUpstreamTerminalFrame(observer.protocol, root) {
		observer.terminal = model
	}
}

func (observer *upstreamModelObserver) actual() (string, string) {
	if observer.terminal != "" {
		return observer.terminal, "terminal"
	}
	if observer.first != "" {
		return observer.first, "first"
	}
	return "", "missing"
}

func (observer *upstreamModelObserver) mismatch(configured string) bool {
	actual, _ := observer.actual()
	return actual != "" && !strings.EqualFold(actual, strings.TrimSpace(configured))
}

func (observer *upstreamModelObserver) log(logger *log.Logger, route config.Route) {
	if logger == nil {
		return
	}
	actual, source := observer.actual()
	expected := upstreamResponseModelForRoute(route)
	configured, ok := validObservedUpstreamModel(expected)
	if !ok {
		configured = "<invalid>"
	}
	mismatch := observer.mismatch(expected)
	if mismatch && !observer.conflict && observer.invalid == 0 &&
		!noteResponseModelMismatch(route.ChannelID, string(observer.protocol), configured, actual) {
		return
	}
	logger.Printf("UP channel=%s response_model upstream=%q configured=%q protocol=%s source=%s mismatch=%t conflict=%t declarations=%d invalid=%d",
		route.ChannelID, actual, configured, observer.protocol, source, mismatch,
		observer.conflict, observer.declarations, observer.invalid)
}

// noteResponseModelMismatch records a benign mismatch pair. It returns true
// the first time this process sees the pair so the line is logged, and false
// for repeats. Conflicts and invalid declarations skip this gate.
func noteResponseModelMismatch(channel, protocol, configured, upstream string) bool {
	key := strings.ToLower(strings.TrimSpace(channel)) + "\x1f" +
		strings.ToLower(strings.TrimSpace(protocol)) + "\x1f" +
		strings.ToLower(strings.TrimSpace(configured)) + "\x1f" +
		strings.ToLower(strings.TrimSpace(upstream))
	responseModelSilenceMu.Lock()
	defer responseModelSilenceMu.Unlock()
	if _, seen := responseModelSilence[key]; seen {
		return false
	}
	if len(responseModelSilence) >= maxResponseModelSilenceKeys {
		return true
	}
	responseModelSilence[key] = struct{}{}
	return true
}

func resetResponseModelSilenceForTest() {
	responseModelSilenceMu.Lock()
	defer responseModelSilenceMu.Unlock()
	responseModelSilence = map[string]struct{}{}
}

// isOfficialGrokCatalogID reports whether name looks like a first-party Grok
// Build catalog ID (grok-4.6, grok-code-fast-1). Custom channel IDs in this
// project use grok4.6-sevnx without the hyphen after "grok".
func isOfficialGrokCatalogID(model string) bool {
	value := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(value, "grok-") && len(value) > len("grok-")
}

func isOfficialXAIRoute(route config.Route) bool {
	return officialHost(route.Host) == "api.x.ai"
}

func officialHost(host string) string {
	host = strings.ToLower(strings.TrimSpace(host))
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	return strings.Trim(host, "[]")
}

func declaredUpstreamModel(protocol wireProtocol, root map[string]any) (any, bool) {
	var nestedField string
	switch protocol {
	case wireResponses:
		nestedField = "response"
	case wireMessages:
		nestedField = "message"
	}
	if nestedField != "" {
		if nested, _ := root[nestedField].(map[string]any); nested != nil {
			if value, exists := nested["model"]; exists {
				return value, true
			}
		}
	}
	value, exists := root["model"]
	return value, exists
}

func validObservedUpstreamModel(raw any) (string, bool) {
	value, ok := raw.(string)
	if !ok {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}

func isUpstreamTerminalFrame(protocol wireProtocol, root map[string]any) bool {
	switch protocol {
	case wireResponses:
		switch stringValue(root["type"]) {
		case "response.completed", "response.failed", "response.incomplete", "error":
			return true
		}
		if response, _ := root["response"].(map[string]any); response != nil {
			switch stringValue(response["status"]) {
			case "completed", "failed", "incomplete":
				return true
			}
		}
	case wireMessages:
		switch stringValue(root["type"]) {
		case "message_stop", "error":
			return true
		case "message_delta":
			delta, _ := root["delta"].(map[string]any)
			return strings.TrimSpace(stringValue(delta["stop_reason"])) != ""
		}
	case wireChatCompletions:
		if root["error"] != nil {
			return true
		}
		for _, rawChoice := range anySlice(root["choices"]) {
			choice, _ := rawChoice.(map[string]any)
			if choice != nil && strings.TrimSpace(stringValue(choice["finish_reason"])) != "" {
				return true
			}
		}
	}
	return false
}
