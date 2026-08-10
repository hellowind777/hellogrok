package proxy

import (
	"io"
	"log"
	"testing"
)

func TestUpstreamTimeoutDefaultsDoNotSetATotalRequestDeadline(t *testing.T) {
	server := New(log.New(io.Discard, "", 0))
	if server.client.Timeout != 0 {
		t.Fatalf("long-lived model streams have a total timeout: %s", server.client.Timeout)
	}
	if server.transport.ResponseHeaderTimeout != defaultUpstreamResponseHeaderTimeout ||
		server.streamIdleTimeout != defaultUpstreamStreamIdleTimeout {
		t.Fatalf("unexpected upstream timeout defaults: headers=%s idle=%s",
			server.transport.ResponseHeaderTimeout, server.streamIdleTimeout)
	}
}
