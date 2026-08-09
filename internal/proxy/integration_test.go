package proxy

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

func startPathTestServer(t *testing.T, s *Server) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.pathLn = listener
	s.PathAddr = listener.Addr().String()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		_ = http.Serve(listener, http.HandlerFunc(s.servePath))
	}()
	t.Cleanup(s.Stop)
}

func facadeRoute(id, backend, model, key, origin string) config.Route {
	parsed, _ := url.Parse(origin)
	return config.Route{
		ChannelID: id, Host: parsed.Host, OriginBase: origin, APIBackend: backend,
		WireModel: model, APIKey: key, AuthScheme: "bearer",
		IncomingAuthScheme: "bearer", SupportsBackendSearch: backend == "responses",
	}
}

func xAPIKeyFacadeRoute(id, backend, model, key, origin string) config.Route {
	route := facadeRoute(id, backend, model, key, origin)
	route.AuthScheme = "x_api_key"
	return route
}

func postFacade(t *testing.T, s *Server, channel string, body []byte, auth string) ([]byte, int) {
	data, status, _ := postFacadeResponse(t, s, channel, body, auth)
	return data, status
}

func postFacadeResponse(t *testing.T, s *Server, channel string, body []byte, auth string) ([]byte, int, http.Header) {
	t.Helper()
	route, ok := s.lookupChannel(channel)
	if !ok {
		t.Fatalf("unknown test route %q", channel)
	}
	protocol, err := routeProtocol(route)
	if route.SupportsBackendSearch {
		protocol = wireResponses
	}
	if err != nil {
		t.Fatal(err)
	}
	return postFacadeProtocol(t, s, channel, protocol, body, auth, nil)
}

func postSearchFacade(t *testing.T, s *Server, channel string, body []byte) ([]byte, int, http.Header) {
	t.Helper()
	return postFacadeProtocol(t, s, channel, wireResponses, body, "", nil)
}

func postFacadeProtocol(
	t *testing.T,
	s *Server,
	channel string,
	protocol wireProtocol,
	body []byte,
	auth string,
	extra http.Header,
) ([]byte, int, http.Header) {
	t.Helper()
	path := protocolPath(protocol)
	request, err := http.NewRequest(http.MethodPost, "http://"+s.PathAddr+"/c/"+url.PathEscape(channel)+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if auth != "" {
		request.Header.Set("Authorization", auth)
	}
	for key, values := range extra {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return data, response.StatusCode, response.Header.Clone()
}

func protocolPath(protocol wireProtocol) string {
	switch protocol {
	case wireResponses:
		return "/responses"
	case wireMessages:
		return "/messages"
	case wireChatCompletions:
		return "/chat/completions"
	default:
		return "/invalid"
	}
}

func buildClientSearchRequest() []byte {
	return buildClientSearchBody("search")
}

func nativeRequestBody(backend string, stream bool) []byte {
	var body string
	switch backend {
	case "responses":
		body = fmt.Sprintf(`{"model":"display","input":"hi","stream":%t}`, stream)
	case "messages":
		body = fmt.Sprintf(`{"model":"display","messages":[{"role":"user","content":"hi"}],"max_tokens":128,"stream":%t}`, stream)
	default:
		body = fmt.Sprintf(`{"model":"display","messages":[{"role":"user","content":"hi"}],"stream":%t}`, stream)
	}
	return []byte(body)
}

func nativeSuccessBody(backend, model, text string) string {
	switch backend {
	case "responses":
		return fmt.Sprintf(`{"id":"resp_1","object":"response","status":"completed","model":%q,"output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":%q,"annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`, model, text)
	case "messages":
		return fmt.Sprintf(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":%q}],"model":%q,"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`, text, model)
	default:
		return fmt.Sprintf(`{"id":"chat_1","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, model, text)
	}
}

func TestServerCanRestartAfterStop(t *testing.T) {
	s := New(log.New(io.Discard, "", 0))
	s.PathAddr = "127.0.0.1:0"
	if err := s.StartPath(); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	if err := s.StartPath(); err != nil {
		t.Fatalf("restart failed: %v", err)
	}
	s.Stop()
}

func TestNativeSessionsUseNativePathsBodiesAndResponses(t *testing.T) {
	for _, backend := range []string{"responses", "messages", "chat_completions"} {
		t.Run(backend, func(t *testing.T) {
			var gotPath, gotAuth string
			var gotBody []byte
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				gotPath, gotAuth = request.URL.Path, request.Header.Get("Authorization")
				gotBody, _ = io.ReadAll(request.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, nativeSuccessBody(backend, "wire-model", "OK"))
			}))
			defer upstream.Close()
			route := facadeRoute("native", backend, "wire-model", "channel-key", upstream.URL+"/v1")
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			data, status := postFacade(t, s, route.ChannelID, nativeRequestBody(backend, false), "Bearer login-oauth")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			protocol := backendProtocol(t, backend)
			if gotPath != "/v1"+protocolPath(protocol) || gotAuth != "Bearer channel-key" {
				t.Fatalf("path=%q auth=%q", gotPath, gotAuth)
			}
			root, err := decodeRequestObject(gotBody)
			if err != nil || root["model"] != "wire-model" {
				t.Fatalf("upstream request=%s err=%v", gotBody, err)
			}
			response, err := decodeJSONMap(data)
			if err != nil {
				t.Fatal(err)
			}
			switch backend {
			case "responses":
				if response["object"] != "response" || response["type"] != nil {
					t.Fatalf("Responses envelope changed protocol: %#v", response)
				}
			case "messages":
				if response["type"] != "message" || response["object"] != nil {
					t.Fatalf("Messages envelope changed protocol: %#v", response)
				}
			case "chat_completions":
				if response["object"] != "chat.completion" || response["output"] != nil {
					t.Fatalf("Chat envelope changed protocol: %#v", response)
				}
			}
		})
	}
}

func TestBackendSearchUsesCurrentChannelAndReturnsResponsesForEveryProviderProtocol(t *testing.T) {
	tests := []struct {
		backend      string
		upstreamBody string
		assertWire   func(*testing.T, map[string]any)
	}{
		{
			backend:      "responses",
			upstreamBody: `{"id":"resp_1","object":"response","status":"completed","model":"wire","output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"current news","sources":[{"type":"url","url":"https://one.example"}]}},{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
			assertWire: func(t *testing.T, root map[string]any) {
				if !hasHostedSearchTool(root) || root["input"] == nil {
					t.Fatalf("Responses hosted search missing: %#v", root)
				}
			},
		},
		{
			backend:      "messages",
			upstreamBody: `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"server_tool_use","id":"ws_1","name":"web_search","input":{"query":"current news"}},{"type":"web_search_tool_result","tool_use_id":"ws_1","content":[{"type":"web_search_result","url":"https://two.example","title":"Two"}]},{"type":"text","text":"answer","citations":[{"type":"web_search_result_location","url":"https://two.example"}]}],"model":"wire","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1,"server_tool_use":{"web_search_requests":1}}}`,
			assertWire: func(t *testing.T, root map[string]any) {
				tools := anySlice(root["tools"])
				if root["messages"] == nil || len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_search_20250305" {
					t.Fatalf("Messages hosted search wire=%#v", root)
				}
			},
		},
		{
			backend:      "chat_completions",
			upstreamBody: `{"id":"chat_1","object":"chat.completion","created":1,"model":"wire","choices":[{"index":0,"message":{"role":"assistant","content":"answer","citations":["https://three.example"]},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2,"web_search_requests":1}}`,
			assertWire: func(t *testing.T, root map[string]any) {
				if root["messages"] == nil || root["web_search_options"] == nil {
					t.Fatalf("Chat hosted search wire=%#v", root)
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			var gotPath string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				gotPath = request.URL.Path
				wire, _ := io.ReadAll(request.Body)
				root, err := decodeRequestObject(wire)
				if err != nil {
					t.Errorf("wire decode: %v", err)
				} else {
					test.assertWire(t, root)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.upstreamBody)
			}))
			defer upstream.Close()

			route := facadeRoute("backend-search", test.backend, "wire", "key", upstream.URL+"/v1")
			route.SupportsBackendSearch = true
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			body := []byte(`{"model":"display","input":"search current news","stream":false}`)
			data, status := postFacade(t, s, route.ChannelID, body, "")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			if gotPath != "/v1"+protocolPath(backendProtocol(t, test.backend)) {
				t.Fatalf("current-channel path=%q", gotPath)
			}
			root, err := decodeJSONMap(data)
			if err != nil || root["object"] != "response" {
				t.Fatalf("Build did not receive Responses: body=%s err=%v", data, err)
			}
			var calls int
			var urls []string
			for _, raw := range anySlice(root["output"]) {
				item, _ := raw.(map[string]any)
				if stringValue(item["type"]) == "web_search_call" {
					calls++
					action, _ := item["action"].(map[string]any)
					urls = mergeUniqueStrings(urls, urlsFromJSON(action["sources"])...)
				}
			}
			if calls != 1 || len(urls) != 1 {
				t.Fatalf("search call/source count mismatch calls=%d urls=%d body=%s", calls, len(urls), data)
			}
		})
	}
}

func TestMessagesHostedSearchStreamConvertsToResponsesSearchEvents(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		wire, _ := io.ReadAll(request.Body)
		root, _ := decodeRequestObject(wire)
		if tools := anySlice(root["tools"]); len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_search_20250305" {
			t.Errorf("Messages hosted search wire=%#v", root)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"ws_1","name":"web_search"}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"news\"}"}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"ws_1","content":[{"type":"web_search_result","url":"https://example.test"}]}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":1}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"answer"}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":2,"delta":{"type":"citations_delta","citation":{"url":"https://example.test"}}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":2}`,
			`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":1}}`,
			`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
		}
		_, _ = io.WriteString(w, strings.Join(frames, "\n\n")+"\n\n")
	}))
	defer upstream.Close()

	route := facadeRoute("messages-search-stream", "messages", "wire", "key", upstream.URL+"/v1")
	route.SupportsBackendSearch = true
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)
	body := []byte(`{"input":"search news","stream":true}`)
	data, status := postFacade(t, s, route.ChannelID, body, "")
	if status != http.StatusOK || !bytes.Contains(data, []byte("response.web_search_call.completed")) ||
		!bytes.Contains(data, []byte("response.completed")) || !bytes.Contains(data, []byte("https://example.test")) {
		t.Fatalf("status=%d stream=%s", status, data)
	}
	for _, native := range [][]byte{[]byte("server_tool_use"), []byte("web_search_tool_result"), []byte("citations_delta")} {
		if bytes.Contains(data, native) {
			t.Fatalf("native Messages frame leaked to Responses consumer: %s", data)
		}
	}
}

func TestWrongSessionProtocolIsRejectedWithoutCallingUpstream(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	route := facadeRoute("messages", "messages", "wire", "key", upstream.URL)
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)
	data, status, header := postFacadeProtocol(t, s, route.ChannelID, wireResponses, []byte(`{"input":"ordinary"}`), "", nil)
	if status != http.StatusBadRequest || header.Get("X-Should-Retry") != "false" ||
		!bytes.Contains(data, []byte("expects")) || calls.Load() != 0 {
		t.Fatalf("status=%d retry=%q calls=%d body=%s", status, header.Get("X-Should-Retry"), calls.Load(), data)
	}
}

func TestDeepSeekChatSearchBridgesToMessagesWithNativeSearchAndXAPIKey(t *testing.T) {
	var gotPath, gotAuthorization, gotAPIKey string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		gotAuthorization = request.Header.Get("Authorization")
		gotAPIKey = request.Header.Get("X-Api-Key")
		gotBody, _ = io.ReadAll(request.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_ds","type":"message","role":"assistant","content":[{"type":"server_tool_use","id":"ws_ds","name":"web_search","input":{"query":"DeepSeek V4"}},{"type":"web_search_tool_result","tool_use_id":"ws_ds","content":[{"type":"web_search_result","url":"https://docs.deepseek.com/v4","title":"DeepSeek V4"}]},{"type":"text","text":"answer","citations":[{"type":"web_search_result_location","url":"https://docs.deepseek.com/v4"}]}],"model":"deepseek-v4-chat","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4,"server_tool_use":{"web_search_requests":1}}}`)
	}))
	defer upstream.Close()

	route := facadeRoute("deepseek-v4-chat", "chat_completions", "deepseek-v4-chat", "channel-key", upstream.URL+"/v1")
	route.Host = "api.deepseek.com"
	route.SupportsBackendSearch = true
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)

	data, status := postFacade(t, s, route.ChannelID, []byte(`{"input":"search DeepSeek V4","stream":false}`), "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if gotPath != "/anthropic/messages" || gotAuthorization != "" || gotAPIKey != "channel-key" {
		t.Fatalf("path=%q authorization=%q x-api-key=%q", gotPath, gotAuthorization, gotAPIKey)
	}
	wire, err := decodeRequestObject(gotBody)
	if err != nil || wire["messages"] == nil {
		t.Fatalf("DeepSeek bridge body=%s err=%v", gotBody, err)
	}
	tools := anySlice(wire["tools"])
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_search_20250305" {
		t.Fatalf("DeepSeek hosted search tool missing: %#v", wire)
	}
	response, err := decodeJSONMap(data)
	if err != nil || response["object"] != "response" ||
		!bytes.Contains(data, []byte("https://docs.deepseek.com/v4")) ||
		!bytes.Contains(data, []byte("web_search_call")) {
		t.Fatalf("DeepSeek search was not converted for Build: body=%s err=%v", data, err)
	}
}

func TestClientSearchAdapterReturnsResponsesForEveryBackend(t *testing.T) {
	tests := []struct {
		backend      string
		upstreamBody string
		assertWire   func(*testing.T, map[string]any)
	}{
		{
			backend:      "responses",
			upstreamBody: `{"id":"resp_search","object":"response","status":"completed","model":"wire","output":[{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"search","sources":[{"type":"url","url":"https://one.example/a"}]}},{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"answer","annotations":[{"type":"url_citation","url":"https://one.example/a","title":"One","start_index":0,"end_index":6}]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
			assertWire: func(t *testing.T, root map[string]any) {
				if !hasHostedSearchTool(root) || root["input"] != "search" {
					t.Fatalf("Responses search wire=%#v", root)
				}
			},
		},
		{
			backend:      "messages",
			upstreamBody: `{"id":"msg_search","type":"message","role":"assistant","content":[{"type":"server_tool_use","id":"ws_1","name":"web_search","input":{"query":"search"}},{"type":"web_search_tool_result","tool_use_id":"ws_1","content":[{"type":"web_search_result","url":"https://two.example/a","title":"Two"}]},{"type":"text","text":"answer","citations":[{"type":"web_search_result_location","url":"https://two.example/a"}]}],"model":"wire","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			assertWire: func(t *testing.T, root map[string]any) {
				tools := anySlice(root["tools"])
				if len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_search_20250305" {
					t.Fatalf("Messages search wire=%#v", root)
				}
			},
		},
		{
			backend:      "chat_completions",
			upstreamBody: `{"id":"chat_search","object":"chat.completion","created":1,"model":"wire","choices":[{"index":0,"message":{"role":"assistant","content":"answer","citations":["https://three.example/a"]},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"web_search_requests":1}}`,
			assertWire: func(t *testing.T, root map[string]any) {
				if root["web_search_options"] == nil || root["search_parameters"] != nil {
					t.Fatalf("Chat search wire=%#v", root)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			var gotPath string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				gotPath = request.URL.Path
				wire, _ := io.ReadAll(request.Body)
				root, err := decodeRequestObject(wire)
				if err != nil {
					t.Errorf("wire decode: %v", err)
				} else {
					test.assertWire(t, root)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.upstreamBody)
			}))
			defer upstream.Close()
			route := facadeRoute("search", test.backend, "wire", "key", upstream.URL+"/v1")
			route.SupportsBackendSearch = false
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			data, status, _ := postSearchFacade(t, s, route.ChannelID, buildClientSearchRequest())
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			if gotPath != "/v1"+protocolPath(backendProtocol(t, test.backend)) {
				t.Fatalf("upstream path=%q", gotPath)
			}
			response, err := decodeJSONMap(data)
			if err != nil || response["object"] != "response" {
				t.Fatalf("client search response=%s err=%v", data, err)
			}
			if err := validateClientSearchOutput(response); err != nil {
				t.Fatal(err)
			}
			var urls []string
			for _, raw := range anySlice(response["output"]) {
				item, _ := raw.(map[string]any)
				if stringValue(item["type"]) == "web_search_call" {
					action, _ := item["action"].(map[string]any)
					urls = mergeUniqueStrings(urls, urlsFromJSON(action["sources"])...)
				}
			}
			if len(urls) == 0 {
				t.Fatalf("verified search sources were lost: %s", data)
			}
		})
	}
}

func TestClientSearchAdapterRejectsAnswersWithoutSearchExecutionEvidence(t *testing.T) {
	tests := []struct {
		backend      string
		upstreamBody string
	}{
		{
			backend:      "responses",
			upstreamBody: `{"id":"resp_plain","object":"response","status":"completed","model":"wire","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ordinary answer","annotations":[]}]}]}`,
		},
		{
			backend:      "messages",
			upstreamBody: `{"id":"msg_plain","type":"message","role":"assistant","content":[{"type":"text","text":"ordinary answer"}],"model":"wire","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		},
		{
			backend:      "chat_completions",
			upstreamBody: `{"id":"chat_plain","object":"chat.completion","created":1,"model":"wire","choices":[{"index":0,"message":{"role":"assistant","content":"ordinary answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.upstreamBody)
			}))
			defer upstream.Close()
			route := facadeRoute("ignored-search", test.backend, "wire", "key", upstream.URL)
			route.SupportsBackendSearch = false
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)

			data, status, header := postSearchFacade(t, s, route.ChannelID, buildClientSearchRequest())
			if status != http.StatusBadGateway || header.Get("X-Should-Retry") != "false" ||
				!bytes.Contains(data, []byte("no completed web_search_call")) {
				t.Fatalf("status=%d retry=%q body=%s", status, header.Get("X-Should-Retry"), data)
			}
		})
	}
}

func TestMessagesParallelToolHistoryIsForwardedUnchangedAndBrokenHistoryStopsLocally(t *testing.T) {
	var calls atomic.Int32
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		gotBody, _ = io.ReadAll(request.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, nativeSuccessBody("messages", "wire", "continued"))
	}))
	defer upstream.Close()
	route := facadeRoute("parallel", "messages", "wire", "key", upstream.URL)
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)
	valid := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"skill","input":{}},{"type":"tool_use","id":"call_2","name":"skill","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_2","content":"second"},{"type":"tool_result","tool_use_id":"call_1","content":"first"}]}],"max_tokens":100}`)
	data, status := postFacade(t, s, route.ChannelID, valid, "")
	if status != http.StatusOK || !bytes.Contains(data, []byte("continued")) {
		t.Fatalf("status=%d body=%s", status, data)
	}
	root, _ := decodeRequestObject(gotBody)
	messages := anySlice(root["messages"])
	if len(messages) != 2 || len(anySlice(messages[0].(map[string]any)["content"])) != 2 || len(anySlice(messages[1].(map[string]any)["content"])) != 2 {
		t.Fatalf("parallel native history changed: %s", gotBody)
	}

	broken := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"skill","input":{}}]}],"max_tokens":100}`)
	data, status, header := postFacadeResponse(t, s, route.ChannelID, broken, "")
	if status != http.StatusBadRequest || header.Get("X-Should-Retry") != "false" || !bytes.Contains(data, []byte("missing tool_result")) {
		t.Fatalf("status=%d retry=%q body=%s", status, header.Get("X-Should-Retry"), data)
	}
	if calls.Load() != 1 {
		t.Fatalf("broken history reached upstream; calls=%d", calls.Load())
	}
}

func TestNativeMessagesJSONRejectsMissingThinkingSignature(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"reason"},{"type":"text","text":"OK"}],"model":"wire","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()

	route := facadeRoute("messages-json", "messages", "wire", "key", upstream.URL)
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, route.ChannelID, nativeRequestBody("messages", false), "")
	if status != http.StatusBadGateway || !bytes.Contains(data, []byte("signature must be a string")) {
		t.Fatalf("status=%d body=%s", status, data)
	}
}

func TestNativeStreamsNormalizeHeartbeatsAndStopAtProtocolTerminal(t *testing.T) {
	tests := []struct {
		backend string
		body    string
		want    string
	}{
		{
			backend: "messages",
			body: "event: keepalive\n" + `data: {"type":"keepalive"}` + "\n\n" +
				`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reason"}}` + "\n\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque-signature"}}` + "\n\n" +
				`data: {"type":"content_block_stop","index":0}` + "\n\n" +
				`data: {"type":"content_block_start","index":1,"content_block":{"type":"text"}}` + "\n\n" +
				`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"OK"}}` + "\n\n" +
				`data: {"type":"content_block_stop","index":1}` + "\n\n" +
				`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"call_1","name":"probe"}}` + "\n\n" +
				`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}` + "\n\n" +
				`data: {"type":"content_block_stop","index":2}` + "\n\n" +
				`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" +
				`data: {"type":"message_stop"}` + "\n\n",
			want: `"type":"message_stop"`,
		},
		{
			backend: "chat_completions",
			body: "data: ping\n\n" +
				`data: {"id":"chat_1","object":"chat.completion.chunk","model":"wire","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"reason","content":"OK"},"finish_reason":"stop"}]}` + "\n\n" +
				"data: [DONE]\n\n",
			want: "data: [DONE]",
		},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			upstreamCanceled := make(chan struct{})
			release := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.body)
				w.(http.Flusher).Flush()
				select {
				case <-request.Context().Done():
					close(upstreamCanceled)
				case <-release:
				}
			}))
			defer func() { close(release); upstream.Close() }()
			route := facadeRoute("stream", test.backend, "wire", "key", upstream.URL)
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			data, status := postFacade(t, s, route.ChannelID, nativeRequestBody(test.backend, true), "")
			if status != http.StatusOK || !bytes.Contains(data, []byte(test.want)) {
				t.Fatalf("status=%d body=%s", status, data)
			}
			if bytes.Contains(data, []byte(`"type":"keepalive"`)) || bytes.Contains(data, []byte("data: ping")) ||
				!bytes.Contains(data, []byte(": keepalive\n\n")) || bytes.Contains(data, []byte("response.completed")) {
				t.Fatalf("native stream was corrupted or translated: %s", data)
			}
			if test.backend == "messages" && (!bytes.Contains(data, []byte(`"signature":""`)) ||
				!bytes.Contains(data, []byte(`"signature":"opaque-signature"`)) ||
				!bytes.Contains(data, []byte(`"content_block":{"text":"","type":"text"}`)) ||
				!bytes.Contains(data, []byte(`"content_block":{"id":"call_1","input":{},"name":"probe","type":"tool_use"}`))) {
				t.Fatalf("Messages stream placeholders were not normalized and preserved: %s", data)
			}
			select {
			case <-upstreamCanceled:
			case <-time.After(time.Second):
				t.Fatal("upstream remained open after native terminal event")
			}
		})
	}
}

func TestResponsesStreamFiltersPrivateKeepaliveAndStopsAtTerminal(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: keepalive\ndata:\n\n"+
			"event: keepalive\n"+`data: {"type":"keepalive"}`+"\n\n"+
			`data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"wire","output":[]}}`+"\n\n"+
			`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"wire","output":[]}}`+"\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-request.Context().Done():
			close(upstreamCanceled)
		case <-release:
		}
	}))
	defer func() { close(release); upstream.Close() }()
	route := facadeRoute("responses-stream", "responses", "wire", "key", upstream.URL)
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, route.ChannelID, nativeRequestBody("responses", true), "")
	if status != http.StatusOK || bytes.Contains(data, []byte(`"type":"keepalive"`)) || bytes.Contains(data, []byte("event: keepalive")) ||
		!bytes.Contains(data, []byte(": keepalive\n\n")) || !bytes.Contains(data, []byte("response.completed")) {
		t.Fatalf("status=%d body=%s", status, data)
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("Responses terminal event did not close upstream")
	}
}

func TestNativeBufferedFallbackStaysInNativeProtocol(t *testing.T) {
	for _, backend := range []string{"responses", "messages", "chat_completions"} {
		t.Run(backend, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, nativeSuccessBody(backend, "wire", "fallback"))
			}))
			defer upstream.Close()
			route := facadeRoute("fallback", backend, "wire", "key", upstream.URL)
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			data, status := postFacade(t, s, route.ChannelID, nativeRequestBody(backend, true), "")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			switch backend {
			case "responses":
				if !bytes.Contains(data, []byte("response.completed")) {
					t.Fatalf("Responses fallback=%s", data)
				}
			case "messages":
				if !bytes.Contains(data, []byte("message_start")) || !bytes.Contains(data, []byte("message_stop")) || bytes.Contains(data, []byte("response.completed")) {
					t.Fatalf("Messages fallback=%s", data)
				}
			case "chat_completions":
				if !bytes.Contains(data, []byte("chat.completion.chunk")) || !bytes.Contains(data, []byte("[DONE]")) || bytes.Contains(data, []byte("response.completed")) {
					t.Fatalf("Chat fallback=%s", data)
				}
			}
		})
	}
}

func TestTruncatedNativeStreamsEmitProtocolNativeErrors(t *testing.T) {
	tests := []struct {
		backend string
		body    string
		want    string
	}{
		{"messages", `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n", `"type":"proxy_stream_error"`},
		{"chat_completions", `data: {"id":"chat_1","object":"chat.completion.chunk","model":"wire","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}` + "\n\n", `"code":"proxy_stream_error"`},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.body)
			}))
			defer upstream.Close()
			route := facadeRoute("truncated", test.backend, "wire", "key", upstream.URL)
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			data, status := postFacade(t, s, route.ChannelID, nativeRequestBody(test.backend, true), "")
			if status != http.StatusOK || !bytes.Contains(data, []byte(test.want)) || bytes.Contains(data, []byte("response.completed")) {
				t.Fatalf("status=%d body=%s", status, data)
			}
		})
	}
}

func TestMalformedSuccessfulEnvelopesAndHTMLAreRejectedWithoutRetry(t *testing.T) {
	for _, backend := range []string{"responses", "messages", "chat_completions"} {
		for _, html := range []bool{false, true} {
			name := backend + "-json"
			if html {
				name = backend + "-html"
			}
			t.Run(name, func(t *testing.T) {
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					if html {
						w.Header().Set("Content-Type", "text/html")
						_, _ = io.WriteString(w, "<html>wrong endpoint</html>")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{}`)
				}))
				defer upstream.Close()
				route := facadeRoute("bad", backend, "wire", "key", upstream.URL)
				s := New(log.New(io.Discard, "", 0))
				s.SetRoutes([]config.Route{route})
				startPathTestServer(t, s)
				data, status, header := postFacadeResponse(t, s, route.ChannelID, nativeRequestBody(backend, false), "")
				if status != http.StatusBadGateway || header.Get("X-Should-Retry") != "false" {
					t.Fatalf("status=%d retry=%q body=%s", status, header.Get("X-Should-Retry"), data)
				}
				if html && !bytes.Contains(data, []byte("base_url API prefix")) {
					t.Fatalf("HTML diagnostic=%s", data)
				}
			})
		}
	}
}

func TestRetryDispositionPreservesUpstreamAndClassifiesDefaults(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		upstream  string
		wantRetry string
	}{
		{"bad request", http.StatusBadRequest, "", "false"},
		{"rate limit", http.StatusTooManyRequests, "", "true"},
		{"recoverable service", http.StatusServiceUnavailable, "", "true"},
		{"upstream veto", http.StatusServiceUnavailable, "false", "false"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "2")
				if test.upstream != "" {
					w.Header().Set("X-Should-Retry", test.upstream)
				}
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, `{"error":{"message":"failure"}}`)
			}))
			defer upstream.Close()
			route := facadeRoute("retry", "responses", "wire", "key", upstream.URL)
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			_, status, header := postFacadeResponse(t, s, route.ChannelID, nativeRequestBody("responses", false), "")
			if status != test.status || header.Get("X-Should-Retry") != test.wantRetry || header.Get("Retry-After") != "2" {
				t.Fatalf("status=%d retry=%q retry-after=%q", status, header.Get("X-Should-Retry"), header.Get("Retry-After"))
			}
		})
	}
}

func TestTransportFailureIsRetryableAndRedactsTarget(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + listener.Addr().String() + "/private-token/v1?api_key=secret"
	_ = listener.Close()
	route := facadeRoute("transport", "responses", "wire", "key", origin)
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)
	data, status, header := postFacadeResponse(t, s, route.ChannelID, nativeRequestBody("responses", false), "")
	if status != http.StatusBadGateway || header.Get("X-Should-Retry") != "true" || bytes.Contains(data, []byte("private-token")) || bytes.Contains(data, []byte("secret")) {
		t.Fatalf("status=%d retry=%q body=%s", status, header.Get("X-Should-Retry"), data)
	}
}

func TestMissingChannelCredentialNeverForwardsLoginOAuth(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	route := facadeRoute("no-key", "responses", "wire", "", upstream.URL)
	route.Host = "api.example.test"
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)
	data, status, header := postFacadeProtocol(t, s, route.ChannelID, wireResponses, nativeRequestBody("responses", false), "Bearer official-login", nil)
	if status != http.StatusUnauthorized || header.Get("X-Should-Retry") != "false" || calls.Load() != 0 || !bytes.Contains(data, []byte("channel-owned credential")) {
		t.Fatalf("status=%d retry=%q calls=%d body=%s", status, header.Get("X-Should-Retry"), calls.Load(), data)
	}
}

func TestSameHostChannelsKeepDistinctCredentialsModelsAndProtocols(t *testing.T) {
	type observed struct{ path, auth, model string }
	var mu sync.Mutex
	var seen []observed
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		root, _ := decodeRequestObject(body)
		mu.Lock()
		seen = append(seen, observed{request.URL.Path, request.Header.Get("Authorization"), stringValue(root["model"])})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(request.URL.Path, "/messages") {
			_, _ = io.WriteString(w, nativeSuccessBody("messages", "model-two", "two"))
		} else {
			_, _ = io.WriteString(w, nativeSuccessBody("responses", "model-one", "one"))
		}
	}))
	defer upstream.Close()
	routes := []config.Route{
		facadeRoute("one", "responses", "model-one", "key-one", upstream.URL+"/v1"),
		facadeRoute("two", "messages", "model-two", "key-two", upstream.URL+"/v1"),
	}
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes(routes)
	startPathTestServer(t, s)
	for _, route := range routes {
		if data, status := postFacade(t, s, route.ChannelID, nativeRequestBody(route.APIBackend, false), ""); status != http.StatusOK {
			t.Fatalf("route=%s status=%d body=%s", route.ChannelID, status, data)
		}
	}
	if len(seen) != 2 || seen[0] != (observed{"/v1/responses", "Bearer key-one", "model-one"}) ||
		seen[1] != (observed{"/v1/messages", "Bearer key-two", "model-two"}) {
		t.Fatalf("observed=%#v", seen)
	}
}

func TestFacadeRejectsBrowserOriginNonLoopbackHostRedirectCookiesAndOversize(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		w.Header().Set("Set-Cookie", "secret=value")
		w.Header().Set("Location", "https://attacker.example")
		w.WriteHeader(http.StatusFound)
	}))
	defer upstream.Close()
	route := facadeRoute("guarded", "responses", "wire", "key", upstream.URL)
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)

	request := httptest.NewRequest(http.MethodPost, "http://127.0.0.1/c/guarded/responses", strings.NewReader(`{}`))
	request.Host = "example.com"
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	s.servePath(recorder, request)
	if recorder.Code != http.StatusMisdirectedRequest {
		t.Fatalf("non-loopback Host status=%d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/c/guarded/responses", strings.NewReader(`{}`))
	request.Host = "127.0.0.1"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://browser.example")
	recorder = httptest.NewRecorder()
	s.servePath(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("Origin status=%d", recorder.Code)
	}

	data, status, header := postFacadeResponse(t, s, route.ChannelID, nativeRequestBody("responses", false), "")
	if status != http.StatusBadGateway || header.Get("Location") != "" || header.Get("Set-Cookie") != "" || !bytes.Contains(data, []byte("redirects are not accepted")) {
		t.Fatalf("redirect status=%d headers=%#v body=%s", status, header, data)
	}
	if calls.Load() != 1 {
		t.Fatalf("unexpected upstream calls=%d", calls.Load())
	}

	request = httptest.NewRequest(http.MethodPost, "http://127.0.0.1/c/guarded/responses", strings.NewReader(`{}`))
	request.Host = "127.0.0.1"
	request.Header.Set("Content-Type", "application/json")
	request.ContentLength = maxFacadeBodyBytes + 1
	recorder = httptest.NewRecorder()
	s.servePath(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status=%d", recorder.Code)
	}
}

func TestFacadeDecodesGzipAndDropsUpstreamCookies(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Set-Cookie", "secret=value")
		writer := gzip.NewWriter(w)
		_, _ = io.WriteString(writer, nativeSuccessBody("responses", "wire", "gzip"))
		_ = writer.Close()
	}))
	defer upstream.Close()
	route := facadeRoute("gzip", "responses", "wire", "key", upstream.URL)
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)
	data, status, header := postFacadeResponse(t, s, route.ChannelID, nativeRequestBody("responses", false), "")
	if status != http.StatusOK || header.Get("Set-Cookie") != "" || !bytes.Contains(data, []byte("gzip")) {
		t.Fatalf("status=%d headers=%#v body=%s", status, header, data)
	}
}

func TestClientSearchAliasRoundTripsNativeProtocols(t *testing.T) {
	tests := []struct {
		backend string
		body    string
	}{
		{"responses", `{"input":"use tool","tools":[{"type":"function","name":"web_search","parameters":{"type":"object"}}]}`},
		{"messages", `{"messages":[{"role":"user","content":"use tool"}],"max_tokens":100,"tools":[{"name":"web_search","input_schema":{"type":"object"}}]}`},
		{"chat_completions", `{"messages":[{"role":"user","content":"use tool"}],"tools":[{"type":"function","function":{"name":"web_search","parameters":{"type":"object"}}}]}`},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				body, _ := io.ReadAll(request.Body)
				root, _ := decodeRequestObject(body)
				name := functionToolName(anySlice(root["tools"])[0].(map[string]any))
				if !isClientWebSearchWireAlias(name) {
					t.Errorf("wire alias=%q body=%s", name, body)
				}
				w.Header().Set("Content-Type", "application/json")
				switch test.backend {
				case "responses":
					_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","model":"wire","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":`+fmt.Sprintf("%q", name)+`,"arguments":"{}","status":"completed"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
				case "messages":
					_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"tool_use","id":"call_1","name":`+fmt.Sprintf("%q", name)+`,"input":{}}],"model":"wire","stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`)
				default:
					_, _ = io.WriteString(w, `{"id":"chat_1","object":"chat.completion","model":"wire","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":`+fmt.Sprintf("%q", name)+`,"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
				}
			}))
			defer upstream.Close()
			route := facadeRoute("alias", test.backend, "wire", "key", upstream.URL)
			route.SupportsBackendSearch = false
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			data, status := postFacade(t, s, route.ChannelID, []byte(test.body), "")
			if status != http.StatusOK || !bytes.Contains(data, []byte(`"name":"web_search"`)) || bytes.Contains(data, []byte(clientWebSearchWireAliasBase)) {
				t.Fatalf("status=%d body=%s", status, data)
			}
		})
	}
}

func TestConcurrentNativeChannelsRemainIsolated(t *testing.T) {
	var failures atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		root, _ := decodeRequestObject(body)
		model := stringValue(root["model"])
		auth := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		if strings.TrimPrefix(model, "model-") != strings.TrimPrefix(auth, "key-") {
			failures.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, nativeSuccessBody("responses", model, "OK"))
	}))
	defer upstream.Close()
	routes := []config.Route{
		facadeRoute("one", "responses", "model-one", "key-one", upstream.URL),
		facadeRoute("two", "responses", "model-two", "key-two", upstream.URL),
	}
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes(routes)
	startPathTestServer(t, s)
	var wg sync.WaitGroup
	for index := 0; index < 20; index++ {
		for _, route := range routes {
			wg.Add(1)
			go func(channel string) {
				defer wg.Done()
				response, err := http.Post("http://"+s.PathAddr+"/c/"+channel+"/responses", "application/json", bytes.NewReader(nativeRequestBody("responses", false)))
				if err != nil {
					failures.Add(1)
					return
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode != http.StatusOK {
					failures.Add(1)
				}
			}(route.ChannelID)
		}
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("isolated concurrent requests failed=%d", failures.Load())
	}
}
