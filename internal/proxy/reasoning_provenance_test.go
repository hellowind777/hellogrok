package proxy

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestReasoningDomainChangesAtEveryIsolationBoundary(t *testing.T) {
	base := config.Route{
		ChannelID:  "claude",
		APIBackend: "messages",
		WireModel:  "claude-sonnet",
		OriginBase: "HTTPS://API.EXAMPLE.test/v1/?b=2&a=1#fragment",
	}
	if reasoningDomain(base) != reasoningDomain(config.Route{
		ChannelID:  "claude",
		APIBackend: "MESSAGES",
		WireModel:  "claude-sonnet",
		OriginBase: "https://api.example.test/v1?a=1&b=2",
	}) {
		t.Fatal("equivalent routes produced different domains")
	}
	mutations := []config.Route{
		{ChannelID: "claude-alt", APIBackend: base.APIBackend, WireModel: base.WireModel, OriginBase: base.OriginBase},
		{ChannelID: base.ChannelID, APIBackend: "responses", WireModel: base.WireModel, OriginBase: base.OriginBase},
		{ChannelID: base.ChannelID, APIBackend: base.APIBackend, WireModel: "claude-opus", OriginBase: base.OriginBase},
		{ChannelID: base.ChannelID, APIBackend: base.APIBackend, WireModel: base.WireModel, OriginBase: "https://other.example/v1"},
	}
	for _, mutation := range mutations {
		if reasoningDomain(base) == reasoningDomain(mutation) {
			t.Fatalf("isolation boundary did not change domain: %+v", mutation)
		}
	}
}

func TestReasoningProjectionIsForeignOnlyAndStructured(t *testing.T) {
	store, err := newReasoningProvenanceStore("")
	if err != nil {
		t.Fatal(err)
	}
	source := config.Route{ChannelID: "source", APIBackend: "messages", WireModel: "model-a", OriginBase: "https://a.example/v1"}
	target := config.Route{ChannelID: "target", APIBackend: "responses", WireModel: "model-b", OriginBase: "https://b.example/v1"}
	store.captureCanonical(reasoningDomain(source), map[string]any{
		"output": []any{
			map[string]any{"type": "reasoning", "encrypted_content": "foreign-signature"},
			map[string]any{"type": "web_search_result", "encrypted_content": "search-state"},
		},
	})
	store.captureCanonical(reasoningDomain(target), map[string]any{
		"type": "reasoning", "encrypted_content": "target-signature",
	})
	if known, _ := store.compatible("search-state", reasoningDomain(source)); known {
		t.Fatal("non-reasoning encrypted content was captured as a signature")
	}

	root, err := decodeRequestObject([]byte(`{
		"input":[
			{"type":"reasoning","id":"foreign","content":[{"type":"reasoning_text","text":"private A"}],"encrypted_content":"foreign-signature"},
			{"type":"message","role":"assistant","content":"visible answer"},
			{"type":"reasoning","id":"target","encrypted_content":"target-signature"},
			{"type":"reasoning","id":"unknown","encrypted_content":"unknown-signature"},
			{"type":"reasoning","id":"plain","content":[{"type":"reasoning_text","text":"portable summary"}]},
			{"type":"web_search_call","encrypted_content":"search-state"}
		]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	stats := filterReasoningInput(root, target, store, keepUnknownReasoning)
	if stats.Opaque != 3 || stats.Compatible != 1 || stats.Unknown != 1 || stats.Dropped != 1 {
		t.Fatalf("unexpected projection stats: %+v", stats)
	}
	encoded, _ := encodeRequestObject(root)
	for _, want := range [][]byte{[]byte("visible answer"), []byte("target-signature"), []byte("unknown-signature"), []byte("portable summary"), []byte("search-state")} {
		if !bytes.Contains(encoded, want) {
			t.Fatalf("projection lost %q: %s", want, encoded)
		}
	}
	if bytes.Contains(encoded, []byte("foreign-signature")) || bytes.Contains(encoded, []byte("private A")) {
		t.Fatalf("foreign reasoning item survived: %s", encoded)
	}

	recoveryStats := filterReasoningInput(root, target, store, dropAllOpaqueReasoning)
	if recoveryStats.Opaque != 2 || recoveryStats.Dropped != 2 {
		t.Fatalf("recovery did not remove remaining opaque items: %+v", recoveryStats)
	}
}

func TestReasoningProvenanceIgnoresNestedToolAndSearchPayloads(t *testing.T) {
	store, err := newReasoningProvenanceStore("")
	if err != nil {
		t.Fatal(err)
	}
	route := config.Route{ChannelID: "source", APIBackend: "responses", WireModel: "model", OriginBase: "https://source.example/v1"}
	domain := reasoningDomain(route)
	store.captureCanonical(domain, map[string]any{
		"output": []any{
			map[string]any{
				"type":   "function_call_output",
				"output": map[string]any{"type": "reasoning", "encrypted_content": "nested-tool-signature"},
			},
			map[string]any{
				"type": "web_search_call",
				"action": map[string]any{
					"sources": []any{map[string]any{"type": "reasoning", "encrypted_content": "nested-search-signature"}},
				},
			},
			map[string]any{"type": "reasoning", "encrypted_content": "canonical-signature"},
		},
	})
	for _, signature := range []string{"nested-tool-signature", "nested-search-signature"} {
		if known, _ := store.compatible(signature, domain); known {
			t.Fatalf("nested application payload was captured as reasoning provenance: %s", signature)
		}
	}
	if known, compatible := store.compatible("canonical-signature", domain); !known || !compatible {
		t.Fatal("canonical reasoning item was not captured")
	}
}

func TestCanonicalReasoningSignaturesAcceptsOnlyProtocolContainers(t *testing.T) {
	want := map[string]bool{
		"direct":   true,
		"output":   true,
		"item":     true,
		"response": true,
		"state":    true,
	}
	fixtures := []any{
		map[string]any{"type": "reasoning", "encrypted_content": "direct"},
		map[string]any{"output": []any{map[string]any{"type": "reasoning", "encrypted_content": "output"}}},
		map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "reasoning", "encrypted_content": "item"}},
		map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{map[string]any{"type": "reasoning", "encrypted_content": "response"}}}},
		[]any{map[string]any{"type": "reasoning", "encrypted_content": "state"}},
	}
	got := map[string]bool{}
	for _, fixture := range fixtures {
		for signature := range canonicalReasoningSignatures(fixture) {
			got[signature] = true
		}
	}
	if len(got) != len(want) {
		t.Fatalf("captured signatures=%v", got)
	}
	for signature := range want {
		if !got[signature] {
			t.Fatalf("missing canonical signature %q: %v", signature, got)
		}
	}
}

func TestReasoningProjectionLeavesRequestsWithoutOpaqueStateUnchanged(t *testing.T) {
	body := []byte(`{"model":"display","messages":[{"role":"assistant","content":"previous","reasoning_content":"plain"},{"role":"user","content":"hello"}],"stream":true}`)
	route := config.Route{ChannelID: "chat", APIBackend: "chat_completions", WireModel: "wire", OriginBase: "https://chat.example/v1"}
	baseline, err := adaptFacadeRequest(body, route, wireChatCompletions)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := newReasoningProvenanceStore("")
	projected, err := adaptFacadeRequestWithReasoning(body, route, wireChatCompletions, store, keepUnknownReasoning)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(baseline.Body, projected.Body) || projected.Reasoning != (reasoningFilterStats{}) {
		t.Fatalf("opaque-free request changed:\nbaseline=%s\nprojected=%s\nstats=%+v", baseline.Body, projected.Body, projected.Reasoning)
	}
}

func TestForeignOpaqueReasoningIsRemovedFromNativeProtocols(t *testing.T) {
	store, _ := newReasoningProvenanceStore("")
	source := config.Route{ChannelID: "source", APIBackend: "responses", WireModel: "source-model", OriginBase: "https://source.example/v1"}
	store.captureCanonical(reasoningDomain(source), map[string]any{"type": "reasoning", "encrypted_content": "source-signature"})
	tests := []struct {
		backend  string
		protocol wireProtocol
		body     string
	}{
		{"responses", wireResponses, `{"input":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"foreign private state"}],"encrypted_content":"source-signature"},{"type":"reasoning","content":[{"type":"reasoning_text","text":"plain summary"}]},{"type":"message","role":"user","content":"continue"}]}`},
		{"messages", wireMessages, `{"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"foreign private state","signature":"source-signature"},{"type":"text","text":"plain summary"}]},{"role":"user","content":"continue"}],"max_tokens":100}`},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			route := config.Route{ChannelID: "target-" + test.backend, APIBackend: test.backend, WireModel: "target-model", OriginBase: "https://target.example/v1"}
			request, err := adaptFacadeRequestWithReasoning([]byte(test.body), route, test.protocol, store, keepUnknownReasoning)
			if err != nil {
				t.Fatal(err)
			}
			if request.Reasoning.Dropped != 1 || bytes.Contains(request.Body, []byte("source-signature")) || bytes.Contains(request.Body, []byte("foreign private state")) {
				t.Fatalf("foreign state reached %s: stats=%+v body=%s", test.backend, request.Reasoning, request.Body)
			}
			if !bytes.Contains(request.Body, []byte("plain summary")) || !bytes.Contains(request.Body, []byte("continue")) {
				t.Fatalf("portable history was lost for %s: %s", test.backend, request.Body)
			}
		})
	}
}

func TestReasoningProvenancePersistsOnlyDigests(t *testing.T) {
	path := filepath.Join(t.TempDir(), reasoningProvenanceFileName)
	store, err := newReasoningProvenanceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	store.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	route := config.Route{ChannelID: "private-channel", APIBackend: "messages", WireModel: "private-model", OriginBase: "https://secret.example/private-token/v1"}
	if added := store.captureCanonical(reasoningDomain(route), map[string]any{"type": "reasoning", "encrypted_content": "raw-secret-signature"}); added != 1 {
		t.Fatalf("captured=%d", added)
	}
	if err := store.flush(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range [][]byte{[]byte("raw-secret-signature"), []byte("private-channel"), []byte("private-model"), []byte("secret.example"), []byte("private-token")} {
		if bytes.Contains(raw, secret) {
			t.Fatalf("provenance file leaked %q: %s", secret, raw)
		}
	}
	reloaded, err := newReasoningProvenanceStore(path)
	if err != nil {
		t.Fatal(err)
	}
	known, compatible := reloaded.compatible("raw-secret-signature", reasoningDomain(route))
	if !known || !compatible {
		t.Fatal("persisted provenance was not restored")
	}
}

func TestReasoningProvenanceCorruptionFailsClosedToUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), reasoningProvenanceFileName)
	if err := os.WriteFile(path, []byte(`{"version":1,"entries":{"not-a-digest":{"domains":["bad"],"seen_unix":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := newReasoningProvenanceStore(path)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("ignored 1 invalid")) {
		t.Fatalf("missing invalid-entry warning: %v", err)
	}
	if len(store.entries) != 0 || !store.dirty {
		t.Fatalf("invalid entry was accepted: entries=%d dirty=%t", len(store.entries), store.dirty)
	}
}

func TestReasoningProvenanceLoadKeepsValidEntriesAndReportsInvalidCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), reasoningProvenanceFileName)
	digest := sha256Hex("valid-signature")
	domain := sha256Hex("valid-domain")
	raw := []byte(`{"version":1,"entries":{"` + digest + `":{"domains":["` + domain + `"],"seen_unix":1},"invalid":{"domains":["bad"],"seen_unix":0}}}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := newReasoningProvenanceStore(path)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("ignored 1 invalid")) {
		t.Fatalf("missing invalid-entry warning: %v", err)
	}
	if known, compatible := store.compatible("valid-signature", domain); !known || !compatible {
		t.Fatal("valid entry was discarded with an invalid neighbor")
	}
}

func TestNewPersistentWithEmptyDataDirStaysInMemory(t *testing.T) {
	t.Chdir(t.TempDir())
	s := NewPersistent(log.New(io.Discard, "", 0), "  ")
	if s.reasoning == nil || s.reasoning.path != "" {
		t.Fatalf("empty data directory produced persistent path %q", s.reasoning.path)
	}
	route := config.Route{ChannelID: "a", APIBackend: "responses", WireModel: "m", OriginBase: "https://a.example/v1"}
	s.captureReasoningProvenance(route, map[string]any{"type": "reasoning", "encrypted_content": "signature"})
	s.flushReasoningProvenance()
	if _, err := os.Stat(reasoningProvenanceFileName); !os.IsNotExist(err) {
		t.Fatalf("empty data directory wrote runtime state in the working directory: %v", err)
	}
}

func TestNewPersistentReportsPartialProvenanceLoadWarning(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, reasoningProvenanceFileName)
	if err := os.WriteFile(path, []byte(`{"version":1,"entries":{"invalid":{"domains":["bad"],"seen_unix":0}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	s := NewPersistent(log.New(&logs, "", 0), dataDir)
	if s.reasoning == nil || len(s.reasoning.entries) != 0 {
		t.Fatal("invalid provenance entry was accepted")
	}
	text := logs.String()
	if !bytes.Contains([]byte(text), []byte("reasoning provenance load warning: ignored 1 invalid")) {
		t.Fatalf("partial load warning was not logged: %s", text)
	}
}

func TestOpaqueReasoningRejectionRequiresStructuredSpecificEvidence(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"encrypted_content was produced by another model"}}`,
		`{"error":{"type":"invalid_request_error","message":"thinking block signature verification failed"}}`,
		`{"error":"could not decrypt reasoning content"}`,
	} {
		if !isOpaqueReasoningRejection(400, []byte(body)) {
			t.Fatalf("rejection was not detected: %s", body)
		}
	}
	for _, body := range []string{
		`{"error":{"message":"model overloaded"}}`,
		`plain text encrypted_content`,
		`{"result":"encrypted_content"}`,
	} {
		if isOpaqueReasoningRejection(400, []byte(body)) {
			t.Fatalf("unrelated error was misclassified: %s", body)
		}
	}
	if isOpaqueReasoningRejection(200, []byte(`{"error":"encrypted_content"}`)) {
		t.Fatal("successful response was classified as a rejection")
	}
}

func TestNewPersistentLoadsProvenanceWithoutLoggingSecrets(t *testing.T) {
	dataDir := t.TempDir()
	var logs bytes.Buffer
	s := NewPersistent(log.New(&logs, "", 0), dataDir)
	route := config.Route{ChannelID: "a", APIBackend: "responses", WireModel: "m", OriginBase: "https://a.example/v1"}
	s.captureReasoningProvenance(route, map[string]any{"type": "reasoning", "encrypted_content": "secret-signature"})
	s.flushReasoningProvenance()
	if strings := logs.String(); bytes.Contains([]byte(strings), []byte("secret-signature")) {
		t.Fatalf("logs exposed an opaque signature: %s", strings)
	}
	reloaded := newServer(log.New(io.Discard, "", 0), filepath.Join(dataDir, reasoningProvenanceFileName))
	known, compatible := reloaded.reasoning.compatible("secret-signature", reasoningDomain(route))
	if !known || !compatible {
		t.Fatal("persistent server did not reload provenance")
	}
}
