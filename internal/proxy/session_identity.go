package proxy

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

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
// header required by OpenCode Go. Existing non-empty target headers always
// win; missing identities are left missing rather than replaced with a
// per-request UUID.
func projectUpstreamSessionHeaders(header http.Header, route config.Route, incoming http.Header, body []byte) {
	if !isOpenCodeGoRoute(route) || strings.TrimSpace(header.Get("x-opencode-session")) != "" {
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

func isOpenCodeGoRoute(route config.Route) bool {
	origin := strings.TrimSpace(route.OriginBase)
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || !strings.EqualFold(u.Hostname(), "opencode.ai") {
		return false
	}
	path := strings.TrimRight(u.Path, "/")
	return path == "/zen/go" || strings.HasPrefix(path, "/zen/go/")
}
