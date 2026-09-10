package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

// sessionIdentity is the stable conversation identity extracted from the
// client request. It is intentionally internal: users configure providers,
// while the proxy adapts provider-specific session names at the boundary.
type sessionIdentity struct {
	value  string
	stable bool
}

// projectUpstreamSessionHeaders adapts a known stable client identity to the
// header used by OpenCode Go and Zen. Existing non-empty target headers always
// win; missing identities are left missing rather than replaced with a
// per-request UUID.
func projectUpstreamSessionHeaders(header http.Header, route config.Route, incoming http.Header, body []byte) {
	if !isOpenCodeRoute(route) || strings.TrimSpace(header.Get("x-opencode-session")) != "" {
		return
	}
	identity := extractSessionIdentity(incoming, body)
	if identity.stable && identity.value != "" {
		header.Set("x-opencode-session", identity.value)
	}
}

func extractSessionIdentity(incoming http.Header, body []byte) sessionIdentity {
	// Preserve an already supplied upstream identity when a caller has one.
	for _, name := range []string{"x-opencode-session", "x-grok-conv-id", "x-grok-session-id", "x-session-id", "session_id"} {
		if value := strings.TrimSpace(incoming.Get(name)); value != "" {
			return sessionIdentity{value: value, stable: true}
		}
	}
	var root struct {
		Metadata struct {
			SessionID string `json:"session_id"`
		} `json:"metadata"`
	}
	if json.Unmarshal(body, &root) == nil {
		if value := strings.TrimSpace(root.Metadata.SessionID); value != "" {
			return sessionIdentity{value: value, stable: true}
		}
	}
	return sessionIdentity{}
}

func isOpenCodeRoute(route config.Route) bool {
	origin := strings.TrimSpace(route.OriginBase)
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || !strings.EqualFold(u.Hostname(), "opencode.ai") {
		return false
	}
	path := strings.TrimRight(u.Path, "/")
	return path == "/zen" || strings.HasPrefix(path, "/zen/")
}

// Build these headers once from the original input, before any protocol
// conversion. Every retry reuses the same identity and channel-owned values.
func openCodeRequestHeaders(route config.Route, incoming http.Header, body []byte) (http.Header, error) {
	result := http.Header{}
	if !isOpenCodeRoute(route) {
		return result, nil
	}
	for _, name := range []string{"x-opencode-session", "User-Agent"} {
		if value := headerValue(route.ExtraHeaders, name); strings.TrimSpace(value) != "" {
			result.Set(name, value)
		}
	}
	projectUpstreamSessionHeaders(result, route, incoming, body)
	if result.Get("User-Agent") == "" {
		if ua := strings.TrimSpace(incoming.Get("User-Agent")); ua != "" {
			result.Set("User-Agent", ua)
		} else {
			ua, err := installedGrokUserAgent()
			if err != nil {
				return nil, err
			}
			result.Set("User-Agent", ua)
		}
	}
	// Missing client identity cannot establish cross-request continuity. Use an
	// isolated operation ID, stable for internal retries, rather than reject the
	// request or merge unrelated conversations into a channel-wide session.
	if result.Get("x-opencode-session") == "" {
		result.Set("x-opencode-session", compatID("operation"))
	}
	return result, nil
}

var installedGrokUserAgent = sync.OnceValues(func() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "grok", "--version").Output()
	if err != nil {
		return "", fmt.Errorf("cannot identify installed Grok Build: %w; configure User-Agent explicitly", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) < 2 || fields[0] != "grok" {
		return "", fmt.Errorf("unrecognized Grok Build version output; configure User-Agent explicitly")
	}
	arch := runtime.GOARCH
	if arch == "amd64" {
		arch = "x86_64"
	} else if arch == "arm64" {
		arch = "aarch64"
	}
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	return fmt.Sprintf("grok-shell/%s (%s; %s)", fields[1], osName, arch), nil
})
