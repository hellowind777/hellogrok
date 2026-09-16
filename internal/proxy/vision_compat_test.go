package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

const visionErrorBody = `{"error":{"type":"invalid_request_error","code":"vision_not_supported","message":"The request model is not multimodal: 'GLM-5.3' does not support image input"}}`

func TestStripVisionContentPerProtocol(t *testing.T) {
	tests := []struct {
		name     string
		protocol wireProtocol
		body     string
		want     int
		wantType string
	}{
		{
			name:     "chat completions",
			protocol: wireChatCompletions,
			body:     `{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]},{"role":"tool","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,BBB"}}]}]}`,
			want:     2,
			wantType: "text",
		},
		{
			name:     "responses",
			protocol: wireResponses,
			body:     `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,AAA"}]}]}`,
			want:     1,
			wantType: "input_text",
		},
		{
			name:     "messages",
			protocol: wireMessages,
			body:     `{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAA"}}]}]}`,
			want:     1,
			wantType: "text",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stripped, removed := stripVisionContent([]byte(test.body), test.protocol)
			if removed != test.want {
				t.Fatalf("removed=%d want=%d body=%s", removed, test.want, stripped)
			}
			if bytes.Contains(stripped, []byte(`"image_url"`)) || bytes.Contains(stripped, []byte(`"input_image"`)) || bytes.Contains(stripped, []byte(`"type":"image"`)) {
				t.Fatalf("image part survived: %s", stripped)
			}
			if !strings.Contains(string(stripped), `"type":"`+test.wantType+`"`) {
				t.Fatalf("placeholder part type %q missing: %s", test.wantType, stripped)
			}
			if !strings.Contains(string(stripped), visionDeniedPlaceholder) {
				t.Fatalf("placeholder text missing: %s", stripped)
			}
		})
	}

	plain := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	stripped, removed := stripVisionContent(plain, wireChatCompletions)
	if removed != 0 || !bytes.Equal(stripped, plain) {
		t.Fatalf("body without images must stay untouched: removed=%d body=%s", removed, stripped)
	}
	if _, removed := stripVisionContent([]byte(`{`), wireChatCompletions); removed != 0 {
		t.Fatalf("invalid JSON must not be rewritten: removed=%d", removed)
	}
}

func TestIsVisionUnsupportedError(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{name: "observed relay error", status: http.StatusBadRequest, body: visionErrorBody, want: true},
		{name: "message only", status: http.StatusBadRequest, body: `{"error":{"message":"model is not multimodal"}}`, want: true},
		{name: "other 400", status: http.StatusBadRequest, body: `{"error":{"type":"invalid_request_error","code":"invalid_tool","message":"unknown tool"}}`, want: false},
		{name: "vision code on 500", status: http.StatusInternalServerError, body: visionErrorBody, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isVisionUnsupportedError(test.status, []byte(test.body)); got != test.want {
				t.Fatalf("got=%t want=%t", got, test.want)
			}
		})
	}
}

func TestVisionStripRetryIsOneShot(t *testing.T) {
	route := facadeRoute("vision", "chat_completions", "wire", "key", "http://upstream.example")
	decider := newRecoveryDecider(route, newAbsorbState(route), context.Background(), func() bool { return false }, nil, nil)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`)
	response := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{}}

	retry, reason := decider.onErrorResponse(response, []byte(visionErrorBody), facadeRequest{Body: body, Protocol: wireChatCompletions}, contextBudgetObservation{}, false)
	if !retry {
		t.Fatalf("retry=%t reason=%q", retry, reason)
	}
	if bytes.Contains(decider.retryBody, []byte(`"image_url"`)) || !bytes.Contains(decider.retryBody, []byte(visionDeniedPlaceholder)) {
		t.Fatalf("retry body not stripped: %s", decider.retryBody)
	}
	var root map[string]any
	if err := json.Unmarshal(decider.retryBody, &root); err != nil {
		t.Fatalf("retry body is not valid JSON: %v", err)
	}

	again, reason := decider.onErrorResponse(response, []byte(visionErrorBody), facadeRequest{Body: decider.retryBody, Protocol: wireChatCompletions}, contextBudgetObservation{}, false)
	if again {
		t.Fatalf("vision retry must be one-shot: reason=%q", reason)
	}

	// A vision rejection without image content in the request has nothing to strip.
	empty := newRecoveryDecider(route, newAbsorbState(route), context.Background(), func() bool { return false }, nil, nil)
	if retry, _ := empty.onErrorResponse(response, []byte(visionErrorBody), facadeRequest{Body: []byte(`{"model":"m","messages":[{"role":"user","content":"a"}]}`), Protocol: wireChatCompletions}, contextBudgetObservation{}, false); retry {
		t.Fatal("vision retry without image content must not fire")
	}
}

func TestVisionStripRetryEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		bodies = append(bodies, body)
		hasImage := bytes.Contains(body, []byte(`"image_url"`))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if hasImage {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, visionErrorBody)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chat_1","object":"chat.completion","model":"wire","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	s := New(log.New(io.Discard, "", 0))
	s.SetRoutes([]config.Route{facadeRoute("vision", "chat_completions", "wire", "key", upstream.URL)})
	startPathTestServer(t, s)

	body := []byte(`{"model":"display","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}],"stream":false}`)

	// Without cross-request memory every image-bearing request pays one
	// rejected attempt and then succeeds via the strip retry.
	for round := 1; round <= 2; round++ {
		data, status := postFacade(t, s, "vision", body, "")
		if status != http.StatusOK {
			t.Fatalf("round %d status=%d body=%s", round, status, data)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 4 {
		t.Fatalf("expected two upstream attempts per request: requests=%d", len(bodies))
	}
	for i, withImage := range []bool{true, false, true, false} {
		if got := bytes.Contains(bodies[i], []byte(`"image_url"`)); got != withImage {
			t.Fatalf("upstream request %d image presence=%t want=%t", i, got, withImage)
		}
		if !withImage && !bytes.Contains(bodies[i], []byte(visionDeniedPlaceholder)) {
			t.Fatalf("upstream request %d missing placeholder: %s", i, bodies[i])
		}
	}
}
