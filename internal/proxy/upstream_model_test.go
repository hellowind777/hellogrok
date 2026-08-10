package proxy

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestUpstreamModelObserverUsesTerminalDeclarationAndDetectsConflicts(t *testing.T) {
	tests := []struct {
		protocol wireProtocol
		frames   []string
		want     string
	}{
		{
			protocol: wireResponses,
			frames: []string{
				`{"type":"response.created","response":{"model":"first-model"}}`,
				`{"type":"response.completed","response":{"model":"terminal-model"}}`,
			},
			want: "terminal-model",
		},
		{
			protocol: wireMessages,
			frames: []string{
				`{"type":"message_start","message":{"model":"first-model"}}`,
				`{"type":"message_stop","model":"terminal-model"}`,
			},
			want: "terminal-model",
		},
		{
			protocol: wireChatCompletions,
			frames: []string{
				`{"model":"first-model","choices":[{"finish_reason":null}]}`,
				`{"model":"terminal-model","choices":[{"finish_reason":"stop"}]}`,
			},
			want: "terminal-model",
		},
	}

	for _, test := range tests {
		t.Run(string(test.protocol), func(t *testing.T) {
			observer := newUpstreamModelObserver(test.protocol)
			for _, frame := range test.frames {
				observer.observeJSON([]byte(frame), false)
			}
			actual, source := observer.actual()
			if actual != test.want || source != "terminal" || !observer.conflict || observer.declarations != 2 {
				t.Fatalf("actual=%q source=%q conflict=%t declarations=%d", actual, source, observer.conflict, observer.declarations)
			}
		})
	}
}

func TestUpstreamModelObserverHandlesCaseAndMissingValues(t *testing.T) {
	caseOnly := newUpstreamModelObserver(wireChatCompletions)
	caseOnly.observeJSON([]byte(`{"model":"GPT-5.6-SOL","choices":[{"finish_reason":null}]}`), false)
	caseOnly.observeJSON([]byte(`{"model":"gpt-5.6-sol","choices":[{"finish_reason":"stop"}]}`), false)
	actual, source := caseOnly.actual()
	if actual != "gpt-5.6-sol" || source != "terminal" || caseOnly.conflict || caseOnly.mismatch("GPT-5.6-SOL") {
		t.Fatalf("case-only declarations were misclassified: %+v", caseOnly)
	}

	invalid := newUpstreamModelObserver(wireResponses)
	invalid.observeJSON([]byte(`{"type":"response.completed","response":{"model":7}}`), false)
	invalid.observeJSON([]byte(`{"type":"response.completed","response":{"model":""}}`), false)
	if actual, source := invalid.actual(); actual != "" || source != "missing" || invalid.invalid != 2 {
		t.Fatalf("invalid declarations were accepted: actual=%q source=%q observer=%+v", actual, source, invalid)
	}
	var logs bytes.Buffer
	invalid.log(log.New(&logs, "", 0), config.Route{ChannelID: "channel", WireModel: "wire"})
	if !strings.Contains(logs.String(), "invalid=2") || !strings.Contains(logs.String(), `upstream=""`) {
		t.Fatalf("missing model diagnostics are incomplete: %s", logs.String())
	}
}

func TestNonStreamingResponsesLogActualUpstreamModelBeforeNormalization(t *testing.T) {
	for _, backend := range []string{"responses", "messages", "chat_completions"} {
		t.Run(backend, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, nativeSuccessBody(backend, "provider-actual", "OK"))
			}))
			defer upstream.Close()

			var logs bytes.Buffer
			route := facadeRoute("observed", backend, "configured-wire", "key", upstream.URL)
			s := New(log.New(&logs, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			data, status := postFacade(t, s, route.ChannelID, nativeRequestBody(backend, false), "")
			if status != http.StatusOK {
				t.Fatalf("status=%d body=%s", status, data)
			}
			line := logs.String()
			if !strings.Contains(line, `response_model upstream="provider-actual" configured="configured-wire"`) ||
				!strings.Contains(line, "mismatch=true") || !strings.Contains(line, "protocol="+backend) {
				t.Fatalf("actual upstream model was not observed for %s: %s", backend, line)
			}
		})
	}
}

func TestStreamingResponsesLogTerminalUpstreamModelForEveryProtocol(t *testing.T) {
	tests := []struct {
		backend string
		body    string
	}{
		{
			backend: "responses",
			body: `data: {"type":"response.created","response":{"id":"resp_model","object":"response","status":"in_progress","model":"first-model","output":[]}}` + "\n\n" +
				`data: {"type":"response.completed","response":{"id":"resp_model","object":"response","status":"completed","model":"terminal-model","output":[]}}` + "\n\n",
		},
		{
			backend: "messages",
			body: `data: {"type":"message_start","message":{"id":"msg_model","type":"message","role":"assistant","content":[],"model":"first-model","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
				`data: {"type":"message_stop","model":"terminal-model"}` + "\n\n",
		},
		{
			backend: "chat_completions",
			body: `data: {"id":"chat_model","object":"chat.completion.chunk","model":"first-model","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n" +
				`data: {"id":"chat_model","object":"chat.completion.chunk","model":"terminal-model","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}]}` + "\n\n" +
				"data: [DONE]\n\n",
		},
	}

	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, test.body)
			}))
			defer upstream.Close()

			var logs bytes.Buffer
			route := facadeRoute("observed-stream", test.backend, "configured-wire", "key", upstream.URL)
			route.SupportsBackendSearch = true
			s := New(log.New(&logs, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			data, status, _ := postFacadeProtocol(t, s, route.ChannelID, wireResponses, nativeRequestBody("responses", true), "", nil)
			if status != http.StatusOK || bytes.Contains(data, []byte("proxy_stream_error")) {
				t.Fatalf("status=%d body=%s", status, data)
			}
			line := logs.String()
			if !strings.Contains(line, `response_model upstream="terminal-model" configured="configured-wire"`) ||
				!strings.Contains(line, "source=terminal") || !strings.Contains(line, "conflict=true") {
				t.Fatalf("terminal upstream model was not observed for %s: %s", test.backend, line)
			}
		})
	}
}
