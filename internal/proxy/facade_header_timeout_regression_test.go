package proxy

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

// A response-header timeout must not cancel the context shared by the retry
// loop. Before the fix, the attempt timer canceled that shared context, so the
// absorb wait returned false immediately and the handler returned without
// writing anything: the client received an empty 200 instead of the designed
// retryable 504, and the absorb retry was a dead path.
func TestFacadeResponseHeaderTimeoutWritesRetryable504(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		// Outlast the response-header deadline (floor 10s + 1s grace = 11s).
		time.Sleep(13 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, nativeSuccessBody("chat_completions", "wire-model", "OK"))
	}))
	defer upstream.Close()

	route := facadeRoute("slow", "chat_completions", "wire-model", "channel-key", upstream.URL+"/v1")
	route.InferenceIdleTimeoutConfigured = true
	route.InferenceIdleTimeoutSecs = 1 // clamped to the 10s floor + 1s grace
	route.AbsorbRetryMaxConfigured = true
	route.AbsorbRetryMaxSecs = 1 // budget smaller than one 2s backoff: exactly one retry
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)

	data, status := postFacadeWithClient(t, s, route.ChannelID, wireChatCompletions, nativeRequestBody("chat_completions", false), "Bearer login-oauth", &http.Client{})
	if status != http.StatusGatewayTimeout {
		t.Fatalf("status=%d body=%s (want retryable 504, not empty 200)", status, data)
	}
	if len(data) == 0 {
		t.Fatal("response body is empty: the handler returned without writing an error")
	}
	// The absorb layer waits ~2s (its minimum backoff), by which the 1s budget
	// is spent, then makes exactly one retry that also times out. Before the fix
	// there was only one call and the client got an empty 200.
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d want 2 (one timed-out attempt + one absorb retry)", got)
	}
}

// The absorb layer must actually retry after a response-header timeout once
// its budget allows, and surface the retried success. Before the fix the
// shared context was already canceled, so the wait returned false and no
// second attempt was ever made.
func TestFacadeResponseHeaderTimeoutAbsorbRetries(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 1 {
			// First attempt: outlast the 11s header deadline so it times out.
			time.Sleep(13 * time.Second)
			return
		}
		// The absorb retry succeeds immediately.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, nativeSuccessBody("chat_completions", "wire-model", "OK"))
	}))
	defer upstream.Close()

	route := facadeRoute("slow", "chat_completions", "wire-model", "channel-key", upstream.URL+"/v1")
	route.InferenceIdleTimeoutConfigured = true
	route.InferenceIdleTimeoutSecs = 1
	route.AbsorbRetryMaxConfigured = true
	route.AbsorbRetryMaxSecs = 60
	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)

	data, status := postFacadeWithClient(t, s, route.ChannelID, wireChatCompletions, nativeRequestBody("chat_completions", false), "Bearer login-oauth", &http.Client{})
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s (want 200 after absorb retry)", status, data)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls=%d want 2 (one timed-out attempt + one successful absorb retry)", got)
	}
}

// postFacadeWithClient mirrors postFacadeProtocol but lets the test supply the
// HTTP client, since the default 5s client timeout would fire before the
// response-header deadline this test exercises.
func postFacadeWithClient(t *testing.T, s *Server, channel string, protocol wireProtocol, body []byte, auth string, client *http.Client) ([]byte, int) {
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
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return data, response.StatusCode
}
