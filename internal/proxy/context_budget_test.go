package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hellowind777/hellogrok/internal/config"
)

func TestClampCompletionForContextError(t *testing.T) {
	errorBody := func(message, errorType string) []byte {
		body, _ := json.Marshal(map[string]any{"error": map[string]any{
			"message": message,
			"type":    errorType,
		}})
		return body
	}
	validMessage := "This model's maximum context length is 1,048,576 tokens. However, you requested 1,048,712 tokens (664,712 in the messages, 384,000 in the completion). Please reduce the length of the messages or completion."

	t.Run("valid responses budget", func(t *testing.T) {
		observation, found := inspectContextBudgetError(http.StatusBadRequest,
			errorBody(validMessage, "invalid_request_error"))
		retry, ok := clampCompletionForContextError(observation,
			[]byte(`{"input":"hi","max_output_tokens":384000}`), wireResponses)
		if !found || !ok || retry.AvailableOutput != 383864 || retry.MessageTokens != 664712 {
			t.Fatalf("retry = %+v, ok=%t", retry, ok)
		}
		root, err := decodeRequestObject(retry.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := positiveJSONUint64(root["max_output_tokens"]); !ok || got != 383864 {
			t.Fatalf("clamped body = %s", retry.Body)
		}
	})

	for _, protocol := range []wireProtocol{wireMessages, wireChatCompletions} {
		t.Run("valid "+string(protocol)+" budget", func(t *testing.T) {
			observation, found := inspectContextBudgetError(http.StatusBadRequest,
				errorBody(validMessage, "invalid_request_error"))
			retry, ok := clampCompletionForContextError(observation,
				[]byte(`{"messages":[],"max_tokens":384000}`), protocol)
			if !found || !ok || retry.AvailableOutput != 383864 {
				t.Fatalf("retry = %+v, ok=%t", retry, ok)
			}
			root, err := decodeRequestObject(retry.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := positiveJSONUint64(root["max_tokens"]); !ok || got != 383864 {
				t.Fatalf("clamped body = %s", retry.Body)
			}
		})
	}

	tests := []struct {
		name      string
		status    int
		message   string
		errorType string
		request   string
		protocol  wireProtocol
	}{
		{"non-400", http.StatusInternalServerError, validMessage, "invalid_request_error", `{"max_output_tokens":384000}`, wireResponses},
		{"spoofed text", http.StatusBadRequest, "prefix: " + validMessage, "invalid_request_error", `{"max_output_tokens":384000}`, wireResponses},
		{"inconsistent requested total", http.StatusBadRequest, "This model's maximum context length is 1048576 tokens. However, you requested 1048711 tokens (664712 in the messages, 384000 in the completion). Please reduce the length of the messages or completion.", "invalid_request_error", `{"max_output_tokens":384000}`, wireResponses},
		{"different outgoing cap", http.StatusBadRequest, validMessage, "invalid_request_error", `{"max_output_tokens":1000}`, wireResponses},
		{"wrong structured error type", http.StatusBadRequest, validMessage, "rate_limit_error", `{"max_output_tokens":384000}`, wireResponses},
		{"unprocessable exact error", http.StatusUnprocessableEntity, validMessage, "invalid_request_error", `{"max_output_tokens":384000}`, wireResponses},
		{"no remaining context", http.StatusBadRequest, "This model's maximum context length is 1048576 tokens. However, you requested 1432576 tokens (1048576 in the messages, 384000 in the completion). Please reduce the length of the messages or completion.", "invalid_request_error", `{"max_output_tokens":384000}`, wireResponses},
		{"wrong protocol field", http.StatusBadRequest, validMessage, "invalid_request_error", `{"max_output_tokens":384000}`, wireMessages},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observation, _ := inspectContextBudgetError(test.status, errorBody(test.message, test.errorType))
			if retry, ok := clampCompletionForContextError(observation, []byte(test.request), test.protocol); ok {
				t.Fatalf("unexpected retry: %+v", retry)
			}
		})
	}
}

func TestInspectContextBudgetErrorSupportsSemanticProviderFormats(t *testing.T) {
	tests := []struct {
		name string
		body string
		want uint64
	}{
		{"nested numeric context window", `{"error":{"code":"context_length_exceeded","metadata":{"context_window":131072}}}`, 131072},
		{"string max context tokens", `{"detail":{"limits":{"max_context_tokens":"262,144"}}}`, 262144},
		{"maximum context length field", `{"error":{"maximum_context_length":1048576}}`, 1048576},
		{"vllm model length field", `{"error":{"type":"BadRequestError","max_model_len":32768}}`, 32768},
		{"semantic message", `{"error":{"message":"The maximum context window: 200000 tokens was exceeded."}}`, 200000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observation, ok := inspectContextBudgetError(http.StatusBadRequest, []byte(test.body))
			if !ok || observation.MaximumTokens != test.want || observation.ExactRequest {
				t.Fatalf("observation=%+v ok=%t", observation, ok)
			}
		})
	}
}

func TestInspectContextBudgetErrorRejectsUntrustworthyMetadata(t *testing.T) {
	tests := []string{
		`{"error":{"context_window":0}}`,
		`{"error":{"context_window":-1}}`,
		`{"error":{"context_window":"18446744073709551616"}}`,
		`{"error":{"max_tokens":131072}}`,
		`{"error":{"context_window":131072,"max_context_tokens":262144}}`,
		`{"error":{"type":"invalid_request_error","context_window":262144,"message":"This model's maximum context length is 131072 tokens. However, you requested 135168 tokens (131072 in the messages, 4096 in the completion). Please reduce the length of the messages or completion."}}`,
		`{"error":{"type":"invalid_request_error","message":"This model's maximum context length is 131072 tokens. However, you requested 135168 tokens (131072 in the messages, 4096 in the completion). Please reduce the length of the messages or completion.","detail":"This model's maximum context length is 262144 tokens. However, you requested 266240 tokens (262144 in the messages, 4096 in the completion). Please reduce the length of the messages or completion."}}`,
		`{"error":{"message":"context limit reached"}}`,
		`not json`,
	}
	for _, body := range tests {
		if observation, ok := inspectContextBudgetError(http.StatusBadRequest, []byte(body)); ok {
			t.Fatalf("untrustworthy metadata accepted: body=%s observation=%+v", body, observation)
		}
	}
	if observation, ok := inspectContextBudgetError(http.StatusInternalServerError,
		[]byte(`{"error":{"context_window":131072}}`)); ok {
		t.Fatalf("server error metadata accepted: %+v", observation)
	}
	if value, ok := positiveJSONUint64(math.Exp2(64)); ok || value != 0 {
		t.Fatalf("overflowing float token count accepted: %d", value)
	}
}

func TestFacadeRetriesContextBudgetOnceWithAvailableCompletion(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		body, _ := io.ReadAll(request.Body)
		root, err := decodeRequestObject(body)
		if err != nil {
			t.Errorf("decode upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		call := calls.Add(1)
		limit, _ := positiveJSONUint64(root["max_output_tokens"])
		if call == 1 {
			if limit != 384000 {
				t.Errorf("first completion limit = %d", limit)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 1048576 tokens. However, you requested 1048712 tokens (664712 in the messages, 384000 in the completion). Please reduce the length of the messages or completion.","type":"invalid_request_error"}}`)
			return
		}
		if limit != 383864 {
			t.Errorf("retry completion limit = %d", limit)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, nativeSuccessBody("responses", "wire", "OK"))
	}))
	defer upstream.Close()

	route := facadeRoute("context-budget", "responses", "wire", "key", upstream.URL)
	server := New(log.New(io.Discard, "", 0))
	server.SetRoutes([]config.Route{route})
	startPathTestServer(t, server)
	data, status, header := postFacadeResponse(t, server, route.ChannelID,
		[]byte(`{"model":"display","input":"hi","max_output_tokens":384000,"stream":false}`), "")
	if status != http.StatusOK || calls.Load() != 2 || !json.Valid(data) ||
		header.Get(grokContextWindowHeader) != "1048576" {
		t.Fatalf("status=%d calls=%d body=%s", status, calls.Load(), data)
	}
}

func TestFacadeAdvertisesDiscoveredWindowWhenNoRetryBudgetRemains(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 1048576 tokens. However, you requested 1432576 tokens (1048576 in the messages, 384000 in the completion). Please reduce the length of the messages or completion.","type":"invalid_request_error"}}`)
	}))
	defer upstream.Close()

	route := facadeRoute("context-no-budget", "responses", "wire", "key", upstream.URL)
	server := New(log.New(io.Discard, "", 0))
	server.SetRoutes([]config.Route{route})
	startPathTestServer(t, server)
	_, status, header := postFacadeResponse(t, server, route.ChannelID,
		[]byte(`{"model":"display","input":"hi","max_output_tokens":384000,"stream":false}`), "")
	if status != http.StatusBadRequest || calls.Load() != 1 || header.Get(grokContextWindowHeader) != "1048576" {
		t.Fatalf("status=%d calls=%d context=%q", status, calls.Load(), header.Get(grokContextWindowHeader))
	}
}

func TestFacadeAdvertisesStructuredWindowWithoutSpeculativeRetry(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = fmt.Fprint(w, `{"error":{"code":"context_length_exceeded","metadata":{"max_context_tokens":"262144"}}}`)
	}))
	defer upstream.Close()

	route := facadeRoute("context-structured", "responses", "wire", "key", upstream.URL)
	server := New(log.New(io.Discard, "", 0))
	server.SetRoutes([]config.Route{route})
	startPathTestServer(t, server)
	_, status, header := postFacadeResponse(t, server, route.ChannelID,
		[]byte(`{"model":"display","input":"hi","max_output_tokens":4096,"stream":false}`), "")
	if status != http.StatusUnprocessableEntity || calls.Load() != 1 || header.Get(grokContextWindowHeader) != "262144" {
		t.Fatalf("status=%d calls=%d context=%q", status, calls.Load(), header.Get(grokContextWindowHeader))
	}
}

func TestFacadeDoesNotLoopContextBudgetRetry(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 1048576 tokens. However, you requested 1048712 tokens (664712 in the messages, 384000 in the completion). Please reduce the length of the messages or completion.","type":"invalid_request_error"}}`)
	}))
	defer upstream.Close()

	route := facadeRoute("context-no-loop", "responses", "wire", "key", upstream.URL)
	server := New(log.New(io.Discard, "", 0))
	server.SetRoutes([]config.Route{route})
	startPathTestServer(t, server)
	_, status := postFacade(t, server, route.ChannelID,
		[]byte(`{"model":"display","input":"hi","max_output_tokens":384000,"stream":false}`), "")
	if status != http.StatusBadRequest || calls.Load() != 2 {
		t.Fatalf("status=%d calls=%d", status, calls.Load())
	}
}

func TestFacadeReasoningRecoveryPreservesContextBudgetClamp(t *testing.T) {
	var calls atomic.Int32
	limits := make([]uint64, 0, 3)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		body, _ := io.ReadAll(request.Body)
		root, err := decodeRequestObject(body)
		if err != nil {
			t.Errorf("decode upstream request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		limit, _ := positiveJSONUint64(root["max_output_tokens"])
		limits = append(limits, limit)
		switch calls.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"message":"This model's maximum context length is 1048576 tokens. However, you requested 1048712 tokens (664712 in the messages, 384000 in the completion). Please reduce the length of the messages or completion.","type":"invalid_request_error"}}`)
		case 2:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":{"type":"invalid_request_error","message":"encrypted_content belongs to another model family"}}`)
		default:
			if strings.Contains(string(body), "legacy-unknown-signature") {
				t.Errorf("reasoning recovery retained opaque signature: %s", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, nativeSuccessBody("responses", "wire", "OK"))
		}
	}))
	defer upstream.Close()

	route := facadeRoute("context-reasoning", "responses", "wire", "key", upstream.URL)
	server := New(log.New(io.Discard, "", 0))
	server.SetRoutes([]config.Route{route})
	startPathTestServer(t, server)
	body := opaqueHistory("legacy-unknown-signature")
	root, err := decodeRequestObject(body)
	if err != nil {
		t.Fatal(err)
	}
	root["max_output_tokens"] = 384000
	body, err = encodeRequestObject(root)
	if err != nil {
		t.Fatal(err)
	}
	data, status := postFacade(t, server, route.ChannelID, body, "")
	if status != http.StatusOK || calls.Load() != 3 || !json.Valid(data) {
		t.Fatalf("status=%d calls=%d body=%s", status, calls.Load(), data)
	}
	wantLimits := []uint64{384000, 383864, 383864}
	if len(limits) != len(wantLimits) {
		t.Fatalf("completion limits = %v", limits)
	}
	for index := range limits {
		if limits[index] != wantLimits[index] {
			t.Fatalf("completion limits = %v, want %v", limits, wantLimits)
		}
	}
}
