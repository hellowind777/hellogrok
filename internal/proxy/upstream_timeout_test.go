package proxy

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestUpstreamTimeoutDefaultsDoNotSetATotalRequestDeadline(t *testing.T) {
	server := New(log.New(io.Discard, "", 0))
	if server.client.Timeout != 0 {
		t.Fatalf("long-lived model streams have a total timeout: %s", server.client.Timeout)
	}
	if server.transport.ResponseHeaderTimeout != 0 || server.bodyIdleTimeout != defaultUpstreamBodyIdleTimeout {
		t.Fatalf("unexpected upstream timeout defaults: headers=%s idle=%s",
			server.transport.ResponseHeaderTimeout, server.bodyIdleTimeout)
	}
	if defaultUpstreamResponseHeaderTimeout != 10*time.Minute+time.Second ||
		defaultUpstreamBodyIdleTimeout != 10*time.Minute+time.Second {
		t.Fatalf("ordinary channels must stay one second beyond Grok Build's 600-second default: headers=%s idle=%s",
			defaultUpstreamResponseHeaderTimeout, defaultUpstreamBodyIdleTimeout)
	}
	if server.deepSeekClient.Timeout != 0 || server.deepSeekTransport.ResponseHeaderTimeout != 0 ||
		server.deepSeekBodyIdleTimeout != defaultDeepSeekBodyIdleTimeout {
		t.Fatalf("unexpected DeepSeek timeout defaults: total=%s headers=%s idle=%s",
			server.deepSeekClient.Timeout, server.deepSeekTransport.ResponseHeaderTimeout, server.deepSeekBodyIdleTimeout)
	}
	if defaultDeepSeekResponseHeaderTimeout <= 10*time.Minute || defaultDeepSeekBodyIdleTimeout <= 10*time.Minute {
		t.Fatalf("DeepSeek timeouts do not cover the documented 10-minute queue: headers=%s idle=%s",
			defaultDeepSeekResponseHeaderTimeout, defaultDeepSeekBodyIdleTimeout)
	}

	client, header, idle := server.upstreamForRoute(config.Route{Host: "api.deepseek.com", WireModel: "deepseek-v4-pro"})
	if client != server.deepSeekClient || header != defaultDeepSeekResponseHeaderTimeout || idle != defaultDeepSeekBodyIdleTimeout {
		t.Fatalf("official DeepSeek route did not select dedicated timeouts: client=%p headers=%s idle=%s", client, header, idle)
	}
	client, header, idle = server.upstreamForRoute(config.Route{Host: "relay.example", WireModel: "deepseek-v4-pro"})
	if client != server.client || header != defaultUpstreamResponseHeaderTimeout || idle != defaultUpstreamBodyIdleTimeout {
		t.Fatalf("relay unexpectedly selected DeepSeek timeouts: client=%p headers=%s idle=%s", client, header, idle)
	}
}

func TestConfiguredInferenceIdleTimeoutMatchesGrokBuildFloorAndAddsGrace(t *testing.T) {
	for _, test := range []struct {
		seconds uint64
		want    time.Duration
	}{
		{seconds: 0, want: 11 * time.Second},
		{seconds: 5, want: 11 * time.Second},
		{seconds: 900, want: 901 * time.Second},
	} {
		route := config.Route{InferenceIdleTimeoutSecs: test.seconds, InferenceIdleTimeoutConfigured: true}
		if got := routeUpstreamIdleTimeout(route, time.Minute); got != test.want {
			t.Fatalf("seconds=%d timeout=%s want=%s", test.seconds, got, test.want)
		}
	}
}

func TestDoUpstreamRequestCancelsWhileWaitingForHeaders(t *testing.T) {
	requestCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
		close(requestCanceled)
	}))
	defer upstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := doUpstreamRequest(upstream.Client(), request, 25*time.Millisecond, cancel)
	if response != nil || !errors.Is(err, errUpstreamResponseHeaderTimeout) {
		t.Fatalf("response=%v error=%v", response, err)
	}
	select {
	case <-requestCanceled:
	case <-time.After(time.Second):
		t.Fatal("upstream request was not canceled after the response-header deadline")
	}
}

func TestDoUpstreamRequestStopsHeaderTimerBeforeReadingLongBody(t *testing.T) {
	headersWritten := make(chan struct{})
	releaseBody := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(headersWritten)
		<-releaseBody
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := doUpstreamRequest(upstream.Client(), request, 25*time.Millisecond, cancel)
	if err != nil {
		t.Fatal(err)
	}
	<-headersWritten
	time.Sleep(75 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatalf("header timer canceled a healthy response body: %v", ctx.Err())
	}
	close(releaseBody)
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || string(data) != "ok" {
		t.Fatalf("body=%q error=%v", data, err)
	}
}
