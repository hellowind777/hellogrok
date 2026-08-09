package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

func opaqueResponsesJSON(model, signature, text string) string {
	formatted := fmt.Sprintf(`{
		"id":"resp_%s","object":"response","status":"completed","model":%q,
		"output":[
			{"type":"reasoning","id":"rs_%s","status":"completed","summary":[],"content":[],"encrypted_content":%q},
			{"type":"message","id":"msg_%s","status":"completed","role":"assistant","content":[{"type":"output_text","text":%q,"annotations":[]}]}
		],
		"usage":{"input_tokens":1,"output_tokens":1}
	}`, model, model, model, signature, model, text)
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(formatted)) != nil {
		return formatted
	}
	return compact.String()
}

func opaqueHistory(signatures ...string) []byte {
	input := make([]any, 0, len(signatures)+2)
	for index, signature := range signatures {
		input = append(input, map[string]any{
			"type": "reasoning", "id": fmt.Sprintf("rs_%d", index),
			"summary": []any{}, "content": []any{}, "encrypted_content": signature,
		})
	}
	input = append(input,
		map[string]any{"type": "message", "role": "assistant", "content": "visible history"},
		map[string]any{"type": "message", "role": "user", "content": "continue"},
	)
	body, _ := encodeRequestObject(map[string]any{"input": input, "stream": false})
	return body
}

func TestReasoningHotSwitchKeepsOnlyTargetDomainAcrossMultipleTurns(t *testing.T) {
	type recorder struct {
		mu     sync.Mutex
		bodies [][]byte
	}
	newUpstream := func(model, signature string, record *recorder) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			record.mu.Lock()
			record.bodies = append(record.bodies, append([]byte(nil), body...))
			record.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, opaqueResponsesJSON(model, signature, model+" answer"))
		}))
	}

	var aRequests, bRequests recorder
	upA := newUpstream("model-a", "signature-a", &aRequests)
	defer upA.Close()
	upB := newUpstream("model-b", "signature-b", &bRequests)
	defer upB.Close()
	routeA := facadeRoute("channel-a", "responses", "model-a", "", upA.URL+"/v1")
	routeB := facadeRoute("channel-b", "responses", "model-b", "", upB.URL+"/v1")

	s := newServer(log.New(io.Discard, "", 0), filepath.Join(t.TempDir(), reasoningProvenanceFileName))
	s.SetRoutes([]config.Route{routeA, routeB})
	startPathTestServer(t, s)

	if data, status := postFacade(t, s, routeA.ChannelID, []byte(`{"input":"first","stream":false}`), ""); status != http.StatusOK {
		t.Fatalf("A first turn status=%d body=%s", status, data)
	}
	if data, status := postFacade(t, s, routeB.ChannelID, opaqueHistory("signature-a"), ""); status != http.StatusOK {
		t.Fatalf("A -> B status=%d body=%s", status, data)
	}
	if data, status := postFacade(t, s, routeB.ChannelID, opaqueHistory("signature-a", "signature-b"), ""); status != http.StatusOK {
		t.Fatalf("B follow-up status=%d body=%s", status, data)
	}
	if data, status := postFacade(t, s, routeA.ChannelID, opaqueHistory("signature-a", "signature-b"), ""); status != http.StatusOK {
		t.Fatalf("B -> A status=%d body=%s", status, data)
	}

	aRequests.mu.Lock()
	aBodies := append([][]byte(nil), aRequests.bodies...)
	aRequests.mu.Unlock()
	bRequests.mu.Lock()
	bBodies := append([][]byte(nil), bRequests.bodies...)
	bRequests.mu.Unlock()
	if len(aBodies) != 2 || len(bBodies) != 2 {
		t.Fatalf("upstream requests A=%d B=%d", len(aBodies), len(bBodies))
	}
	if bytes.Contains(bBodies[0], []byte("signature-a")) || bytes.Contains(bBodies[1], []byte("signature-a")) {
		t.Fatalf("A signature reached B: first=%s second=%s", bBodies[0], bBodies[1])
	}
	if !bytes.Contains(bBodies[1], []byte("signature-b")) {
		t.Fatalf("B lost its own signature on a same-model turn: %s", bBodies[1])
	}
	if !bytes.Contains(aBodies[1], []byte("signature-a")) || bytes.Contains(aBodies[1], []byte("signature-b")) {
		t.Fatalf("switching back to A projected the wrong history: %s", aBodies[1])
	}
	if !bytes.Contains(aBodies[1], []byte("visible history")) || !bytes.Contains(bBodies[1], []byte("visible history")) {
		t.Fatal("ordinary history was removed during hot switching")
	}
}

func TestMessagesReasoningProvenanceSurvivesProxyRestart(t *testing.T) {
	messagesUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"private","signature":"claude-signature"},{"type":"text","text":"answer"}],"model":"claude-real","stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer messagesUpstream.Close()
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, reasoningProvenanceFileName)
	sourceRoute := facadeRoute("claude", "messages", "claude-real", "", messagesUpstream.URL+"/v1")
	source := newServer(log.New(io.Discard, "", 0), path)
	source.SetRoutes([]config.Route{sourceRoute})
	startPathTestServer(t, source)
	if data, status := postFacade(t, source, sourceRoute.ChannelID, nativeRequestBody("messages", false), ""); status != http.StatusOK {
		t.Fatalf("Messages source status=%d body=%s", status, data)
	}
	source.Stop()

	var targetBody []byte
	targetUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, opaqueResponsesJSON("target-real", "target-signature", "continued"))
	}))
	defer targetUpstream.Close()
	targetRoute := facadeRoute("target", "responses", "target-real", "", targetUpstream.URL+"/v1")
	target := newServer(log.New(io.Discard, "", 0), path)
	target.SetRoutes([]config.Route{targetRoute})
	startPathTestServer(t, target)
	if data, status := postFacade(t, target, targetRoute.ChannelID, opaqueHistory("claude-signature"), ""); status != http.StatusOK {
		t.Fatalf("post-restart switch status=%d body=%s", status, data)
	}
	if bytes.Contains(targetBody, []byte("claude-signature")) || !bytes.Contains(targetBody, []byte("visible history")) {
		t.Fatalf("persistent provenance did not project the request: %s", targetBody)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("claude-signature")) || bytes.Contains(raw, []byte("claude-real")) {
		t.Fatalf("persistent provenance leaked source data: %s", raw)
	}
}

func TestStreamingReasoningIsCapturedBeforeTheNextModelTurn(t *testing.T) {
	for _, backend := range []string{"responses", "messages"} {
		t.Run(backend, func(t *testing.T) {
			const signature = "stream-signature"
			sourceUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if backend == "responses" {
					response := opaqueResponsesJSON("stream-source", signature, "streamed")
					_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
					return
				}
				frames := []string{
					`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"stream-source","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`,
					`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
					`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"private"}}`,
					`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"stream-signature"}}`,
					`{"type":"content_block_stop","index":0}`,
					`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`,
					`{"type":"message_stop"}`,
				}
				for _, frame := range frames {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", frame)
				}
			}))
			defer sourceUpstream.Close()
			var targetBody []byte
			targetUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				targetBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, opaqueResponsesJSON("stream-target", "target-signature", "ok"))
			}))
			defer targetUpstream.Close()
			sourceRoute := facadeRoute("stream-source-"+backend, backend, "stream-source", "", sourceUpstream.URL+"/v1")
			targetRoute := facadeRoute("stream-target-"+backend, "responses", "stream-target", "", targetUpstream.URL+"/v1")
			s := New(log.New(io.Discard, "", 0))
			s.SetRoutes([]config.Route{sourceRoute, targetRoute})
			startPathTestServer(t, s)
			sourceBody := nativeRequestBody(backend, true)
			terminal := []byte("response.completed")
			if backend == "messages" {
				terminal = []byte("message_stop")
			}
			if data, status := postFacade(t, s, sourceRoute.ChannelID, sourceBody, ""); status != http.StatusOK || !bytes.Contains(data, terminal) {
				t.Fatalf("source stream status=%d body=%s", status, data)
			}
			if data, status := postFacade(t, s, targetRoute.ChannelID, opaqueHistory(signature), ""); status != http.StatusOK {
				t.Fatalf("stream switch status=%d body=%s", status, data)
			}
			if bytes.Contains(targetBody, []byte(signature)) || !bytes.Contains(targetBody, []byte("visible history")) {
				t.Fatalf("stream provenance was unavailable on next turn: %s", targetBody)
			}
		})
	}
}

func TestUnknownReasoningRejectionGetsExactlyOneCleanReplay(t *testing.T) {
	for _, test := range []struct {
		name         string
		alwaysFail   bool
		wantStatus   int
		wantAttempts int32
	}{
		{name: "recovery succeeds", wantStatus: http.StatusOK, wantAttempts: 2},
		{name: "recovery fails once", alwaysFail: true, wantStatus: http.StatusBadRequest, wantAttempts: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			var attempts atomic.Int32
			var secondBody []byte
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempt := attempts.Add(1)
				body, _ := io.ReadAll(r.Body)
				if attempt == 2 {
					secondBody = append([]byte(nil), body...)
				}
				if bytes.Contains(body, []byte("legacy-unknown-signature")) || test.alwaysFail {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-Should-Retry", "true")
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"encrypted_content belongs to another model family"}}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, opaqueResponsesJSON("target", "new-signature", "recovered"))
			}))
			defer up.Close()
			s := New(log.New(io.Discard, "", 0))
			route := facadeRoute("target", "responses", "target", "", up.URL+"/v1")
			s.SetRoutes([]config.Route{route})
			startPathTestServer(t, s)
			body := []byte(`{
				"input":[
					{"type":"reasoning","encrypted_content":"legacy-unknown-signature","content":[{"type":"reasoning_text","text":"legacy private"}]},
					{"type":"message","role":"assistant","content":"visible history"},
					{"type":"function_call","call_id":"call_1","name":"save","arguments":"{}"},
					{"type":"function_call_output","call_id":"call_1","output":"saved"},
					{"type":"message","role":"user","content":"continue"}
				],
				"stream":false
			}`)
			data, status, headers := postFacadeResponse(t, s, route.ChannelID, body, "")
			if status != test.wantStatus || attempts.Load() != test.wantAttempts {
				t.Fatalf("status=%d attempts=%d body=%s", status, attempts.Load(), data)
			}
			if bytes.Contains(secondBody, []byte("legacy-unknown-signature")) || bytes.Contains(secondBody, []byte("legacy private")) {
				t.Fatalf("clean replay retained opaque state: %s", secondBody)
			}
			for _, want := range [][]byte{[]byte("visible history"), []byte("call_1"), []byte("saved"), []byte("continue")} {
				if !bytes.Contains(secondBody, want) {
					t.Fatalf("clean replay lost %q: %s", want, secondBody)
				}
			}
			if test.alwaysFail && headers.Get("X-Should-Retry") != "false" {
				t.Fatalf("final deterministic failure remained retryable: %q", headers.Get("X-Should-Retry"))
			}
		})
	}
}

func TestOpaqueHistoryDoesNotRetryUnrelatedErrors(t *testing.T) {
	var attempts atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"model overloaded"}}`)
	}))
	defer up.Close()
	s := New(log.New(io.Discard, "", 0))
	route := facadeRoute("target", "responses", "target", "", up.URL+"/v1")
	s.SetRoutes([]config.Route{route})
	startPathTestServer(t, s)
	_, status := postFacade(t, s, route.ChannelID, opaqueHistory("unknown-signature"), "")
	if status != http.StatusServiceUnavailable || attempts.Load() != 1 {
		t.Fatalf("status=%d attempts=%d", status, attempts.Load())
	}
}

func TestReasoningRecoveryErrorMatcherDoesNotInspectUserText(t *testing.T) {
	data := []byte(`{"input":"please explain encrypted_content"}`)
	if isOpaqueReasoningRejection(http.StatusBadRequest, data) {
		t.Fatal("ordinary request-shaped JSON was treated as an upstream error")
	}
	if strings.Contains(string(data), "signature") {
		t.Fatal("test fixture unexpectedly changed")
	}
}
