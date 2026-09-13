package proxy

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestGrokBuildClientHeadersReplaceHelloGrokUserAgent(t *testing.T) {
	header := http.Header{"User-Agent": []string{"hellogrok/0.1.23"}}
	incoming := http.Header{
		"User-Agent":               []string{"grok-shell/1.2.3 (windows; x86_64)"},
		"X-Grok-Client-Identifier": []string{"grok-shell"},
		"X-Grok-Client-Version":    []string{"1.2.3"},
	}
	applyGrokBuildClientHeaders(header, incoming)
	if header.Get("User-Agent") != "grok-shell/1.2.3 (windows; x86_64)" {
		t.Fatalf("ua=%q", header.Get("User-Agent"))
	}
	if header.Get("X-Grok-Client-Identifier") != "grok-shell" {
		t.Fatalf("id=%q", header.Get("X-Grok-Client-Identifier"))
	}
	if header.Get("X-Grok-Client-Version") != "1.2.3" {
		t.Fatalf("version=%q", header.Get("X-Grok-Client-Version"))
	}
}

func TestGrokBuildClientHeadersSynthesizeIdentityWithoutIncoming(t *testing.T) {
	header := http.Header{}
	applyGrokBuildClientHeaders(header, http.Header{})
	if !isGrokShellUserAgent(header.Get("User-Agent")) {
		t.Fatalf("ua=%q", header.Get("User-Agent"))
	}
	if header.Get("X-Grok-Client-Identifier") != grokShellClientIdentifier {
		t.Fatalf("id=%q", header.Get("X-Grok-Client-Identifier"))
	}
}

func TestOpenCodeHeadersSurviveProtocolConversion(t *testing.T) {
	for _, backend := range []string{"responses", "messages", "chat_completions"} {
		t.Run(backend, func(t *testing.T) {
			route := facadeRoute("probe", backend, "wire", "test-key", "https://opencode.ai/zen/go/v1")
			route.SupportsBackendSearch = true
			route.APIBackendConfigured = true
			s := New(log.New(io.Discard, "", 0))
			defer s.Stop()
			calls := 0
			s.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.Header.Get("x-opencode-session") != "child-session" {
					t.Error("original metadata identity lost")
				}
				if req.Header.Get("User-Agent") != "grok-shell/test (windows; x86_64)" {
					t.Error("missing client identification")
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(nativeSuccessBody(backend, "wire", "OK")))}, nil
			})
			r := httptest.NewRequest("POST", "http://127.0.0.1/c/probe/responses", strings.NewReader(`{"model":"probe","input":"hello","max_output_tokens":128,"metadata":{"session_id":"child-session"}}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("User-Agent", "grok-shell/test (windows; x86_64)")
			w := httptest.NewRecorder()
			s.forwardFacade(w, r, route, wireResponses)
			if w.Code != 200 || calls != 1 {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, calls, w.Body.String())
			}
		})
	}
}

func TestOpenCodeIdentityPolicy(t *testing.T) {
	incoming := http.Header{"User-Agent": []string{"grok-shell/test"}}
	goRoute := config.Route{OriginBase: "https://opencode.ai/zen/go/v1"}
	if h, err := openCodeRequestHeaders(goRoute, incoming, nil); err != nil || h.Get("x-opencode-session") == "" {
		t.Fatal("identity-free Go request must receive an operation ID")
	}
	zen := config.Route{OriginBase: "https://opencode.ai/zen/v1"}
	if h, err := openCodeRequestHeaders(zen, incoming, nil); err != nil || h.Get("x-opencode-session") == "" {
		t.Fatal("identity-free Zen request must receive an operation ID")
	}
	goRoute.ExtraHeaders = map[string]string{"x-opencode-session": "explicit", "User-Agent": "custom-agent/1"}
	h, err := openCodeRequestHeaders(goRoute, http.Header{"X-Grok-Conv-Id": []string{"parent"}}, nil)
	if err != nil || h.Get("x-opencode-session") != "explicit" || h.Get("User-Agent") != "custom-agent/1" {
		t.Fatalf("explicit overrides lost: %v", err)
	}
}

func TestOpenCodeNativeRequestsWithoutIdentityReachUpstream(t *testing.T) {
	for _, backend := range []string{"responses", "messages", "chat_completions"} {
		for _, path := range []string{"/zen/v1", "/zen/go/v1"} {
			t.Run(backend+path, func(t *testing.T) {
				route := facadeRoute("probe", backend, "wire", "key", "https://opencode.ai"+path)
				route.SupportsBackendSearch = false
				s := New(log.New(io.Discard, "", 0))
				defer s.Stop()
				s.absorbSleep = func(context.Context, time.Duration) bool { return true }
				var ids []string
				s.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					ids = append(ids, req.Header.Get("x-opencode-session"))
					status, body := 200, nativeSuccessBody(backend, "wire", "OK")
					if len(ids)%2 == 1 {
						status, body = 500, `{"error":{"message":"busy"}}`
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
				})
				for i := 0; i < 2; i++ {
					r := httptest.NewRequest("POST", "http://127.0.0.1/c/probe"+protocolPath(backendProtocol(t, backend)), strings.NewReader(string(nativeRequestBody(backend, false))))
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("User-Agent", "grok-shell/test")
					w := httptest.NewRecorder()
					s.forwardFacade(w, r, route, backendProtocol(t, backend))
					if w.Code != 200 {
						t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
					}
				}
				if len(ids) != 4 || ids[0] == "" || ids[0] != ids[1] || ids[2] != ids[3] || ids[0] == ids[2] {
					t.Fatal("incorrect operation identity lifetime")
				}
			})
		}
	}
}

func TestOpenCodeStandaloneSearchHeadersAndRetries(t *testing.T) {
	for _, backend := range []string{"responses", "messages", "chat_completions"} {
		for _, path := range []string{"/zen/v1", "/zen/go/v1"} {
			t.Run(backend+path, func(t *testing.T) {
				route := facadeRoute("search", backend, "wire", "key", "https://opencode.ai"+path)
				route.SupportsBackendSearch = false
				route.ChatSearchDialect = config.ChatSearchDialectWebSearchOptions
				// The off default lets the deterministic 400 pass through
				// after the busy 500 is absorbed; only the opt-in balanced
				// tier would absorb the 400 as well.
				s := New(log.New(io.Discard, "", 0))
				defer s.Stop()
				s.absorbSleep = func(context.Context, time.Duration) bool { return true }
				var identities []string
				s.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					identities = append(identities, req.Header.Get("x-opencode-session"))
					if req.Header.Get("User-Agent") != "grok-shell/test" {
						t.Error("client identity overwritten")
					}
					status := http.StatusBadRequest
					if len(identities)%2 == 1 {
						status = http.StatusInternalServerError
					}
					return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"fixture"}}`))}, nil
				})
				for i := 0; i < 2; i++ {
					r := httptest.NewRequest("POST", "http://127.0.0.1/c/search/responses", strings.NewReader(string(buildClientSearchRequest())))
					r.Header.Set("Content-Type", "application/json")
					r.Header.Set("User-Agent", "grok-shell/test")
					w := httptest.NewRecorder()
					s.forwardFacade(w, r, route, wireResponses)
					if w.Code != 400 {
						t.Fatalf("unexpected status %d", w.Code)
					}
				}
				if len(identities) != 4 {
					t.Fatalf("upstream attempts=%d, want 4", len(identities))
				}
				if identities[0] == "" || identities[0] != identities[1] || identities[2] != identities[3] || identities[0] == identities[2] {
					t.Fatal("search identity scope or retry stability broken")
				}
			})
		}
	}
}

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

func TestProjectUpstreamSessionHeadersOnlyTargetsOpenCode(t *testing.T) {
	for _, route := range []config.Route{
		{OriginBase: "https://api.x.ai/v1"},
		{OriginBase: "https://opencode.ai/zen/v1"},
		{OriginBase: "https://opencode.ai/zen/go/v1"},
		{OriginBase: "https://opencode.ai.evil.example/zen/go/v1"},
		{OriginBase: "https://opencode.ai/zen-other/v1"},
	} {
		header := http.Header{}
		projectUpstreamSessionHeaders(header, route, http.Header{"X-Grok-Conv-Id": []string{"conv-1"}}, nil)
		want := ""
		if route.OriginBase == "https://opencode.ai/zen/go/v1" || route.OriginBase == "https://opencode.ai/zen/v1" {
			want = "conv-1"
		}
		if got := header.Get("x-opencode-session"); got != want {
			t.Fatalf("origin=%q x-opencode-session=%q want %q", route.OriginBase, got, want)
		}
	}
}
