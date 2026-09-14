package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestSummarizeUpstreamErrorJSONShapes(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantTyp  string
		wantCode string
		wantMsg  string
	}{
		{
			name:     "openai",
			body:     `{"error":{"type":"invalid_request_error","code":"invalid_model","message":"model not found"}}`,
			wantTyp:  "invalid_request_error",
			wantCode: "invalid_model",
			wantMsg:  "model not found",
		},
		{
			name:    "anthropic",
			body:    `{"type":"error","error":{"type":"invalid_request_error","message":"messages: extra field"}}`,
			wantTyp: "invalid_request_error",
			wantMsg: "messages: extra field",
		},
		{
			name:     "responses failed",
			body:     `{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"rate_limit_exceeded","message":"Concurrency limit exceeded"}}}`,
			wantTyp:  "response.failed",
			wantCode: "rate_limit_exceeded",
			wantMsg:  "Concurrency limit exceeded",
		},
		{
			name:    "string error",
			body:    `{"error":"upstream overloaded"}`,
			wantMsg: "upstream overloaded",
		},
		{
			name:    "html",
			body:    "<!DOCTYPE html><html><body>Just a moment</body></html>",
			wantTyp: "html",
			wantMsg: "<!DOCTYPE html><html><body>Just a moment</body></html>",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := summarizeUpstreamError([]byte(test.body))
			if got.Type != test.wantTyp || got.Code != test.wantCode || got.Message != test.wantMsg {
				t.Fatalf("got %+v want type=%q code=%q message=%q", got, test.wantTyp, test.wantCode, test.wantMsg)
			}
		})
	}
}

func TestSanitizeLogTextRedactsSecretsAndTruncates(t *testing.T) {
	got := sanitizeLogText("denied bearer sk-live-abcdefghijklmnopqrstuvwxyz api_key=secret-value rest")
	if strings.Contains(got, "sk-live") || strings.Contains(got, "secret-value") || strings.Contains(got, "bearer sk") {
		t.Fatalf("secret leaked: %q", got)
	}
	if !strings.Contains(got, "[redacted]") {
		t.Fatalf("expected redaction marks: %q", got)
	}
	if got := sanitizeLogText("invalid api key"); got != "invalid api key" {
		t.Fatalf("phrase without a secret value was rewritten: %q", got)
	}
	long := strings.Repeat("你", maxLoggedErrorMessageRunes+8)
	clipped := sanitizeLogText(long)
	if !strings.HasSuffix(clipped, "…") {
		t.Fatalf("missing truncation mark: %q", clipped)
	}
	if strings.Count(clipped, "你") != maxLoggedErrorMessageRunes {
		t.Fatalf("truncated rune count=%d", strings.Count(clipped, "你"))
	}
}

func TestIsClientStreamAbort(t *testing.T) {
	if isClientStreamAbort(nil, false, nil) {
		t.Fatal("empty close must not look like a client abort")
	}
	if !isClientStreamAbort(context.Canceled, false, nil) {
		t.Fatal("context.Canceled must be a client abort")
	}
	if !isClientStreamAbort(fmt.Errorf("read: %w", context.Canceled), false, nil) {
		t.Fatal("wrapped context.Canceled must be a client abort")
	}
	if isClientStreamAbort(errUpstreamBodyIdleTimeout, false, nil) {
		t.Fatal("idle timeout must stay a stream failure")
	}
	if isClientStreamAbort(io.ErrUnexpectedEOF, false, nil) {
		t.Fatal("upstream truncation must stay a stream failure")
	}
	if !isClientStreamAbort(nil, true, nil) {
		t.Fatal("a failed client write is a client abort")
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if !isClientStreamAbort(nil, false, &http.Response{Request: req}) {
		t.Fatal("a canceled request context must be a client abort even after a clean EOF")
	}
}

func TestHTTPErrorBodyIsLoggedRedacted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"invalid_tool","message":"unknown tool; bearer sk-abcdefghijklmnopqrstuvwxyz"}}`)
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	route := facadeRoute("error-log", "responses", "wire", "key", upstream.URL)
	s := New(log.New(&logs, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)

	data, status := postFacade(t, s, route.ChannelID, nativeRequestBody("responses", false), "")
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", status, data)
	}
	out := logs.String()
	if !strings.Contains(out, `error source=http`) ||
		!strings.Contains(out, `status=400`) ||
		!strings.Contains(out, `type="invalid_request_error"`) ||
		!strings.Contains(out, `code="invalid_tool"`) ||
		!strings.Contains(out, `unknown tool`) {
		t.Fatalf("missing error summary: %s", out)
	}
	if strings.Contains(out, "sk-abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("secret leaked into the log: %s", out)
	}
}

func TestResponsesFailedEventIsLogged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			`data: {"type":"response.failed","response":{"id":"resp_1","object":"response","status":"failed","model":"wire","output":[],"error":{"code":"server_error","message":"inference failed"}}}`+"\n\n")
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	route := facadeRoute("failed-log", "responses", "wire", "key", upstream.URL)
	s := New(log.New(&logs, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)

	data, status := postFacade(t, s, route.ChannelID, nativeRequestBody("responses", true), "")
	if status != http.StatusOK || !bytes.Contains(data, []byte("response.failed")) {
		t.Fatalf("status=%d body=%s", status, data)
	}
	out := logs.String()
	if !strings.Contains(out, `error source=response.failed`) ||
		!strings.Contains(out, `code="server_error"`) ||
		!strings.Contains(out, `inference failed`) {
		t.Fatalf("missing response.failed summary: %s", out)
	}
}

func TestClientAbortDoesNotInjectStreamError(t *testing.T) {
	tests := []struct {
		name     string
		backend  string
		incoming string
		search   bool
		frame    string
	}{
		{
			name:     "responses",
			backend:  "responses",
			incoming: "responses",
			search:   true,
			frame:    `data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"wire","output":[]}}` + "\n\n",
		},
		{
			name:     "messages",
			backend:  "messages",
			incoming: "messages",
			frame:    `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n",
		},
		{
			name:     "chat",
			backend:  "chat_completions",
			incoming: "chat_completions",
			frame:    `data: {"id":"chat_1","object":"chat.completion.chunk","model":"wire","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n",
		},
		{
			name:     "translated-messages",
			backend:  "messages",
			incoming: "responses",
			search:   true,
			frame:    `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"wire","usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n",
		},
		{
			name:     "translated-chat",
			backend:  "chat_completions",
			incoming: "responses",
			search:   true,
			frame:    `data: {"id":"chat_1","object":"chat.completion.chunk","model":"wire","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}` + "\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			started := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				_, _ = io.WriteString(w, test.frame)
				flusher.Flush()
				close(started)
				<-request.Context().Done()
			}))
			defer upstream.Close()

			var logs bytes.Buffer
			route := facadeRoute("abort-"+test.name, test.backend, "wire", "key", upstream.URL)
			route.SupportsBackendSearch = test.search
			s := New(log.New(&logs, "", 0))
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := nativeRequestBody(test.incoming, true)
			path := protocolPath(wireProtocol(test.incoming))
			if test.incoming == "chat_completions" {
				path = protocolPath(wireChatCompletions)
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+s.PathAddr+"/c/"+url.PathEscape(route.ChannelID)+path, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			type clientResult struct {
				data []byte
				err  error
			}
			got := make(chan clientResult, 1)
			go func() {
				resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(request)
				if err != nil {
					got <- clientResult{err: err}
					return
				}
				data, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				got <- clientResult{data: data}
			}()
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream never started the stream")
			}
			time.Sleep(50 * time.Millisecond)
			cancel()
			var data []byte
			select {
			case result := <-got:
				if result.err != nil && !errors.Is(result.err, context.Canceled) && !strings.Contains(result.err.Error(), "canceled") {
					t.Fatalf("client request: %v", result.err)
				}
				data = result.data
			case <-time.After(3 * time.Second):
				t.Fatal("client request did not finish after cancel")
			}
			if bytes.Contains(data, []byte("proxy_stream_error")) {
				t.Fatalf("client abort injected a stream error: %s", data)
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				out := logs.String()
				if strings.Contains(out, "aborted by client") &&
					!strings.Contains(out, "ended without") &&
					!strings.Contains(out, "ended without message_stop") &&
					!strings.Contains(out, "ended without [DONE]") {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("missing abort log or still reported a missing terminal: %s", out)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

func TestCleanTruncationStillErrors(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"wire","output":[]}}`+"\n\n")
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	route := facadeRoute("truncate-log", "responses", "wire", "key", upstream.URL)
	s := New(log.New(&logs, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)
	data, status := postFacade(t, s, route.ChannelID, nativeRequestBody("responses", true), "")
	if status != http.StatusOK || !bytes.Contains(data, []byte("proxy_stream_error")) {
		t.Fatalf("status=%d body=%s", status, data)
	}
	out := logs.String()
	if !strings.Contains(out, "ended without a Responses terminal event") || strings.Contains(out, "aborted by client") {
		t.Fatalf("clean truncation was misclassified: %s", out)
	}
}
