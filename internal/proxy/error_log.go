package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxLoggedErrorMessageRunes = 512
	maxLoggedErrorTokenRunes   = 64
)

// logSecretPattern strips credentials that sometimes leak into provider error
// text (bearer tokens, key assignments, sk- prefixes). It does not match
// phrases such as "invalid api key" that carry no secret value.
var logSecretPattern = regexp.MustCompile(`(?i)(?:bearer\s+[a-z0-9._\-+=/]+|(?:api[_-]?key|access[_-]?token|secret|password)\s*[:=]\s*\S+|sk-[a-z0-9]{8,})`)

type upstreamErrorSummary struct {
	Type    string
	Code    string
	Message string
}

func (s *Server) logUpstreamError(channel, source string, status int, data []byte) {
	if s == nil || s.log == nil {
		return
	}
	summary := summarizeUpstreamError(data)
	s.log.Printf("UP channel=%s error source=%s status=%d type=%q code=%q message=%q",
		channel, source, status, summary.Type, summary.Code, summary.Message)
}

func (s *Server) logUpstreamErrorObject(channel, source string, status int, root map[string]any) {
	if s == nil || s.log == nil {
		return
	}
	summary := summarizeUpstreamErrorObject(root)
	s.log.Printf("UP channel=%s error source=%s status=%d type=%q code=%q message=%q",
		channel, source, status, summary.Type, summary.Code, summary.Message)
}

func summarizeUpstreamError(data []byte) upstreamErrorSummary {
	if root, err := decodeJSONMap(data); err == nil {
		return summarizeUpstreamErrorObject(root)
	}
	summary := upstreamErrorSummary{Message: sanitizeLogText(string(data))}
	if looksLikeHTML(data) {
		summary.Type = "html"
	}
	return summary
}

func summarizeUpstreamErrorObject(root map[string]any) upstreamErrorSummary {
	if root == nil {
		return upstreamErrorSummary{}
	}
	errorBody, _ := root["error"].(map[string]any)
	if errorBody == nil {
		if response, _ := root["response"].(map[string]any); response != nil {
			errorBody, _ = response["error"].(map[string]any)
		}
	}
	src := errorBody
	if src == nil {
		src = root
	}
	message := firstNonEmpty(
		anyString(src["message"]),
		anyString(src["detail"]),
		anyString(root["message"]),
		anyString(root["detail"]),
	)
	if message == "" {
		message = anyString(root["error"])
		if message == "" {
			if response, _ := root["response"].(map[string]any); response != nil {
				message = anyString(response["error"])
			}
		}
	}
	summary := upstreamErrorSummary{
		Type:    clipLogToken(sanitizeLogText(firstNonEmpty(anyString(src["type"]), anyString(root["type"])))),
		Code:    clipLogToken(sanitizeLogText(firstNonEmpty(anyString(src["code"]), anyString(root["code"])))),
		Message: sanitizeLogText(message),
	}
	if summary.Type == "error" && summary.Code != "" {
		summary.Type = ""
	}
	return summary
}

func looksLikeHTML(data []byte) bool {
	trimmed := strings.TrimSpace(string(data))
	if len(trimmed) < 5 {
		return false
	}
	lower := strings.ToLower(trimmed)
	return strings.HasPrefix(lower, "<!doctype html") || strings.HasPrefix(lower, "<html")
}

func anyString(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return strings.TrimSpace(typed.String())
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func sanitizeLogText(value string) string {
	value = logSecretPattern.ReplaceAllString(value, "[redacted]")
	value = strings.Join(strings.Fields(value), " ")
	return clipLogRunes(value, maxLoggedErrorMessageRunes)
}

func clipLogToken(value string) string {
	return clipLogRunes(value, maxLoggedErrorTokenRunes)
}

func clipLogRunes(value string, max int) string {
	if max <= 0 || utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max]) + "…"
}

func isClientStreamAbort(err error, writeFailed bool, response *http.Response) bool {
	if writeFailed {
		return true
	}
	if response != nil && response.Request != nil && response.Request.Context().Err() != nil {
		return true
	}
	return errors.Is(err, context.Canceled)
}
