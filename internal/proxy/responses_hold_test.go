package proxy

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestClassifyResponsesHoldEvent(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want responsesHoldAction
	}{
		{name: "created", raw: `{"type":"response.created","response":{"id":"resp_1","status":"in_progress"}}`, want: responsesHoldBuffer},
		{name: "in_progress", raw: `{"type":"response.in_progress"}`, want: responsesHoldBuffer},
		{name: "queued", raw: `{"type":"response.queued"}`, want: responsesHoldBuffer},
		{name: "rate limit failed", raw: `{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"Concurrency limit exceeded"}}}`, want: responsesHoldAbsorb},
		{name: "overloaded error event", raw: `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`, want: responsesHoldAbsorb},
		{name: "auth failed", raw: `{"type":"response.failed","response":{"status":"failed","error":{"code":"invalid_api_key","message":"bad key"}}}`, want: responsesHoldRelease},
		{name: "text delta", raw: `{"type":"response.output_text.delta","delta":"hi"}`, want: responsesHoldRelease},
		{name: "completed", raw: `{"type":"response.completed","response":{"status":"completed","output":[]}}`, want: responsesHoldRelease},
		{name: "failed without error", raw: `{"type":"response.failed","response":{"status":"failed","output":[]}}`, want: responsesHoldRelease},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event, err := decodeJSONMap([]byte(test.raw))
			if err != nil {
				t.Fatal(err)
			}
			if got := classifyResponsesHoldEvent(event); got != test.want {
				t.Fatalf("action=%v want %v", got, test.want)
			}
		})
	}
}

func TestRetryableResponsesStreamFailureStatus(t *testing.T) {
	event, err := decodeJSONMap([]byte(`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"Concurrency limit exceeded","retry_after":7}}}`))
	if err != nil {
		t.Fatal(err)
	}
	status, retryAfter, body, ok := retryableResponsesStreamFailure(event)
	if !ok || status != http.StatusTooManyRequests || retryAfter != 7 {
		t.Fatalf("status=%d retryAfter=%d ok=%t body=%s", status, retryAfter, ok, body)
	}
	if !bytes.Contains(body, []byte(`"code":"rate_limit_exceeded"`)) {
		t.Fatalf("error JSON lost the provider code: %s", body)
	}
}

func TestShouldAbsorbResponsesFailureHonorsDisabledWindow(t *testing.T) {
	event, err := decodeJSONMap([]byte(`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"rate limit"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if shouldAbsorbResponsesFailure(event, nil) {
		t.Fatal("nil absorb state must not withhold")
	}
	disabled := newAbsorbState(config.Route{AbsorbRetryMaxConfigured: true, AbsorbRetryMaxSecs: 0})
	if shouldAbsorbResponsesFailure(event, disabled) {
		t.Fatal("explicit zero window must stream the failed event")
	}
	enabled := newAbsorbState(config.Route{})
	if !shouldAbsorbResponsesFailure(event, enabled) {
		t.Fatal("default window must withhold a retryable failed event")
	}
}

func TestClassifyStructuredRetrySeesNestedResponsesErrorAndConcurrency(t *testing.T) {
	classified, transient := classifyStructuredRetry([]byte(`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"Concurrency limit exceeded"}}}`))
	if !classified || !transient {
		t.Fatalf("classified=%t transient=%t", classified, transient)
	}
	classified, transient = classifyStructuredRetry([]byte(`{"error":{"message":"Concurrency limit exceeded for account"}}`))
	if !classified || !transient {
		t.Fatalf("message-only concurrency classified=%t transient=%t", classified, transient)
	}
}

func responsesFailedSSE(code, message string) string {
	return `data: {"type":"response.failed","response":{"id":"resp_1","object":"response","status":"failed","model":"wire","output":[],"error":{"code":"` + code + `","message":"` + message + `"}}}` + "\n\n"
}

func responsesCreatedSSE() string {
	return `data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"wire","output":[]}}` + "\n\n"
}

func responsesCompletedSSE() string {
	return `data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"wire","output":[]}}` + "\n\n" +
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"ok"}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"wire","output":[{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}]}}` + "\n\n"
}

func TestRetryableResponsesFailedIsAbsorbedThenRecovers(t *testing.T) {
	var served int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.Header().Set("Content-Type", "text/event-stream")
		if served == 1 {
			_, _ = io.WriteString(w, responsesFailedSSE("rate_limit_exceeded", "Concurrency limit exceeded"))
			return
		}
		_, _ = io.WriteString(w, responsesCompletedSSE())
	})
	server, calls := newBreakerTestServer(t, handler)
	stubAbsorbClock(server)

	data, status := postFacade(t, server, "breaker-channel", nativeRequestBody("responses", true), "")
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, data)
	}
	if bytes.Contains(data, []byte("response.failed")) || bytes.Contains(data, []byte("proxy_stream_error")) {
		t.Fatalf("retryable failed event leaked to the client: %s", data)
	}
	if !bytes.Contains(data, []byte("response.completed")) || !bytes.Contains(data, []byte(`"text":"ok"`)) {
		t.Fatalf("recovered stream missing completed output: %s", data)
	}
	if got := *calls; got != 2 {
		t.Fatalf("upstream calls=%d want 2", got)
	}
}

func TestCreatedThenRetryableFailedIsAbsorbed(t *testing.T) {
	var served int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served++
		w.Header().Set("Content-Type", "text/event-stream")
		if served == 1 {
			_, _ = io.WriteString(w, responsesCreatedSSE()+responsesFailedSSE("rate_limit_exceeded", "rate limit exceeded, retry later"))
			return
		}
		_, _ = io.WriteString(w, responsesCompletedSSE())
	})
	server, calls := newBreakerTestServer(t, handler)
	stubAbsorbClock(server)

	data, status := postFacade(t, server, "breaker-channel", nativeRequestBody("responses", true), "")
	if status != http.StatusOK || bytes.Contains(data, []byte("response.failed")) || *calls != 2 {
		t.Fatalf("status=%d calls=%d body=%s", status, *calls, data)
	}
}

func TestNonRetryableResponsesFailedIsStreamed(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesFailedSSE("invalid_api_key", "bad key"))
	})
	server, calls := newBreakerTestServer(t, handler)
	stubAbsorbClock(server)

	data, status := postFacade(t, server, "breaker-channel", nativeRequestBody("responses", true), "")
	if status != http.StatusOK || !bytes.Contains(data, []byte("response.failed")) || *calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", status, *calls, data)
	}
}

func TestDisabledAbsorbStreamsRetryableResponsesFailed(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesFailedSSE("rate_limit_exceeded", "Concurrency limit exceeded"))
	})
	server, calls := newBreakerTestServer(t, handler)
	setBreakerTestRoute(t, server, func(route *config.Route) {
		route.AbsorbRetryMaxConfigured = true
		route.AbsorbRetryMaxSecs = 0
	})

	data, status := postFacade(t, server, "breaker-channel", nativeRequestBody("responses", true), "")
	if status != http.StatusOK || !bytes.Contains(data, []byte("response.failed")) || *calls != 1 {
		t.Fatalf("disabled absorb must stream the failed event, status=%d calls=%d body=%s", status, *calls, data)
	}
}

func TestHeldCreatedIsFlushedOnFirstContent(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesCompletedSSE())
	})
	server, calls := newBreakerTestServer(t, handler)

	data, status := postFacade(t, server, "breaker-channel", nativeRequestBody("responses", true), "")
	if status != http.StatusOK || *calls != 1 {
		t.Fatalf("status=%d calls=%d body=%s", status, *calls, data)
	}
	if !bytes.Contains(data, []byte("response.created")) ||
		!bytes.Contains(data, []byte("response.output_text.delta")) ||
		!bytes.Contains(data, []byte("response.completed")) {
		t.Fatalf("held created was not flushed with content: %s", data)
	}
}
