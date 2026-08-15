package proxy

import (
	"log"
	"strings"

	"github.com/hellowind777/hellogrok/internal/config"
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
	actual, source := observer.actual()
	expected := responseModelForRoute(route)
	configured, ok := validObservedUpstreamModel(expected)
	if !ok {
		configured = "<invalid>"
	}
	logger.Printf("UP channel=%s response_model upstream=%q configured=%q protocol=%s source=%s mismatch=%t conflict=%t declarations=%d invalid=%d",
		route.ChannelID, actual, configured, observer.protocol, source, observer.mismatch(expected),
		observer.conflict, observer.declarations, observer.invalid)
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
