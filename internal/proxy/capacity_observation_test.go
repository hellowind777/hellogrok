package proxy

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hellowind777/hellogrok/internal/capacity"
	"github.com/hellowind777/hellogrok/internal/config"
)

func TestFacadeObservesRequestAndTrustedResponseCapacity(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(grokContextWindowHeader, "262144")
		w.Header().Set(grokMaxCompletionTokensHeader, "16384")
		_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"wire","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	route := facadeRoute("capacity", "responses", "wire", "key", upstream.URL)
	server := New(log.New(io.Discard, "", 0))
	observed := make(chan capacity.Observation, 4)
	server.SetCapacityObserver(func(channel string, observation capacity.Observation) {
		if channel != route.ChannelID {
			t.Errorf("channel=%q", channel)
		}
		observed <- observation
	})
	server.SetRoutes([]config.Route{route})
	startPathTestServer(t, server)
	_, status, _ := postFacadeResponse(t, server, route.ChannelID,
		[]byte(`{"model":"display","input":"hi","max_output_tokens":4096,"stream":false}`), "")
	if status != http.StatusOK {
		t.Fatalf("status=%d", status)
	}
	first, second := <-observed, <-observed
	if first.MaxCompletionTokens != 4096 || first.CompletionSource != capacity.SourceRequest {
		t.Fatalf("request observation=%+v", first)
	}
	if second.ContextWindow != 262144 || second.MaxCompletionTokens != 16384 ||
		second.ContextSource != capacity.SourceResponseHeader || second.CompletionSource != capacity.SourceResponseHeader {
		t.Fatalf("response observation=%+v", second)
	}
}

func TestCapacityObservationRejectsInvalidHeaders(t *testing.T) {
	header := http.Header{}
	header.Set(grokContextWindowHeader, "-1")
	header.Set(grokMaxCompletionTokensHeader, "4294967296")
	if got := capacityObservationFromHeaders(header); got != (capacity.Observation{}) {
		t.Fatalf("invalid headers produced observation: %+v", got)
	}
}
