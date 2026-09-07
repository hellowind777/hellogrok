package proxy

import (
	"net/http"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestProjectUpstreamSessionHeaders(t *testing.T) {
	route := config.Route{OriginBase: "https://opencode.ai/zen/go/v1"}
	tests := []struct {
		name     string
		incoming http.Header
		body     string
		initial  string
		want     string
	}{
		{name: "conversation preferred", incoming: http.Header{"X-Grok-Conv-Id": []string{"conv-1"}, "X-Grok-Session-Id": []string{"session-1"}}, want: "conv-1"},
		{name: "session fallback", incoming: http.Header{"X-Grok-Session-Id": []string{"session-1"}}, want: "session-1"},
		{name: "metadata fallback", body: `{"metadata":{"session_id":"meta-1"}}`, want: "meta-1"},
		{name: "existing target wins", incoming: http.Header{"X-Grok-Conv-Id": []string{"conv-1"}}, initial: "explicit", want: "explicit"},
		{name: "empty identity stays absent", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			header := http.Header{}
			if test.initial != "" {
				header.Set("x-opencode-session", test.initial)
			}
			projectUpstreamSessionHeaders(header, route, test.incoming, []byte(test.body))
			if got := header.Get("x-opencode-session"); got != test.want {
				t.Fatalf("x-opencode-session=%q want %q", got, test.want)
			}
		})
	}
}

func TestProjectUpstreamSessionHeadersOnlyTargetsOpenCodeGo(t *testing.T) {
	for _, route := range []config.Route{
		{OriginBase: "https://api.x.ai/v1"},
		{OriginBase: "https://opencode.ai/zen/v1"},
		{OriginBase: "https://opencode.ai/zen/go/v1"},
	} {
		header := http.Header{}
		projectUpstreamSessionHeaders(header, route, http.Header{"X-Grok-Conv-Id": []string{"conv-1"}}, nil)
		want := ""
		if route.OriginBase == "https://opencode.ai/zen/go/v1" {
			want = "conv-1"
		}
		if got := header.Get("x-opencode-session"); got != want {
			t.Fatalf("origin=%q x-opencode-session=%q want %q", route.OriginBase, got, want)
		}
	}
}
