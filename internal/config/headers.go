package config

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

var protectedRouteHeaders = map[string]struct{}{
	"accept-encoding":     {},
	"connection":          {},
	"content-length":      {},
	"content-type":        {},
	"host":                {},
	"keep-alive":          {},
	"proxy-authorization": {},
	"proxy-connection":    {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

func validateRouteHeaders(headers map[string]string) error {
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	seen := make(map[string]string, len(keys))
	for _, key := range keys {
		identity := strings.ToLower(key)
		if previous, exists := seen[identity]; exists {
			return fmt.Errorf("duplicate HTTP header %q conflicts with %q", key, previous)
		}
		seen[identity] = key
		if err := validateRouteHeader(key, headers[key]); err != nil {
			return err
		}
	}
	return nil
}

func validateRouteHeader(name, value string) error {
	if !validHeaderName(name) {
		return fmt.Errorf("invalid HTTP header name %q", name)
	}
	lowerName := strings.ToLower(name)
	if _, protected := protectedRouteHeaders[lowerName]; protected {
		return fmt.Errorf("HTTP header %q is controlled by the proxy and cannot be configured", name)
	}
	if !validHeaderValue(value) {
		return fmt.Errorf("invalid HTTP header value for %q", name)
	}
	return nil
}

func configuredRouteHeaders(raw any, field string) (map[string]string, error) {
	values, err := configuredStringMap(raw, field)
	if err != nil {
		return nil, err
	}
	if err := validateRouteHeaders(values); err != nil {
		return nil, fmt.Errorf("%s: %w", field, err)
	}
	return canonicalizeRouteHeaders(values), nil
}

func configuredEnvHTTPHeaders(raw any, field string) (map[string]string, error) {
	values, err := configuredStringMap(raw, field)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make(map[string]string, len(values))
	seen := make(map[string]string, len(values))
	for _, name := range keys {
		envName := strings.TrimSpace(values[name])
		identity := strings.ToLower(name)
		if previous, exists := seen[identity]; exists {
			return nil, fmt.Errorf("%s: duplicate HTTP header %q conflicts with %q", field, name, previous)
		}
		seen[identity] = name
		if err := validateRouteHeader(name, ""); err != nil {
			return nil, fmt.Errorf("%s: %w", field, err)
		}
		if envName == "" || strings.ContainsRune(envName, '=') {
			return nil, fmt.Errorf("%s: invalid environment variable name for HTTP header %q", field, name)
		}
		result[http.CanonicalHeaderKey(name)] = envName
	}
	return result, nil
}

func configuredStringMap(raw any, field string) (map[string]string, error) {
	if raw == nil {
		return map[string]string{}, nil
	}
	result := map[string]string{}
	switch values := raw.(type) {
	case map[string]any:
		for key, rawValue := range values {
			value, ok := rawValue.(string)
			if !ok {
				return nil, fmt.Errorf("%s.%s must be a string", field, key)
			}
			result[key] = value
		}
	case map[string]string:
		for key, value := range values {
			result[key] = value
		}
	default:
		return nil, fmt.Errorf("%s must be a table of string values", field)
	}
	return result, nil
}

func canonicalizeRouteHeaders(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[http.CanonicalHeaderKey(key)] = value
	}
	return result
}

func validHeaderName(value string) bool {
	for index := 0; index < len(value); index++ {
		if !isHeaderTokenByte(value[index]) {
			return false
		}
	}
	return value != ""
}

func isHeaderTokenByte(value byte) bool {
	if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' {
		return true
	}
	switch value {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}

func validHeaderValue(value string) bool {
	for index := 0; index < len(value); index++ {
		current := value[index]
		if current == '\t' {
			continue
		}
		if current < 0x20 || current == 0x7f {
			return false
		}
	}
	return true
}
