package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hellowind777/hellogrok/internal/config"
)

// These tests exercise the recoveryDecider decision matrix directly, without
// the full facade: each decide call must pick the same plan the main loop
// would have executed inline.

const deciderProbeBody = `{"model":"display","input":"hi","max_output_tokens":64,"stream":false}`

func newTestDecider(route config.Route, absorb *absorbState, ctx context.Context, stopped func() bool) *recoveryDecider {
	if stopped == nil {
		stopped = func() bool { return true }
	}
	return newRecoveryDecider(route, absorb, ctx, stopped, nil, []byte(deciderProbeBody))
}

// stubDeciderAbsorb returns an absorbState with a virtual clock whose sleeps
// advance instantly. exhausted=true seeds a started time beyond the budget so
// the next wait returns false immediately.
func stubDeciderAbsorb(route config.Route, exhausted bool) *absorbState {
	absorb := newAbsorbState(route)
	now := time.Unix(1_000_000, 0)
	absorb.now = func() time.Time { return now }
	absorb.sleep = func(ctx context.Context, wait time.Duration) bool { return ctx.Err() == nil }
	if exhausted {
		absorb.started = now.Add(-absorb.budget - time.Second)
	}
	return absorb
}

func newTestResponse(status int, header http.Header) *http.Response {
	return &http.Response{StatusCode: status, Header: header}
}

// decideErrorPath mirrors the main loop's error path: the one-shot
// recoveries first, then the retry disposition.
func decideErrorPath(t *testing.T, decider *recoveryDecider, response *http.Response, data []byte, request facadeRequest) (recoveryPlan, string) {
	t.Helper()
	retry, reason := decider.onErrorResponse(response, data, request, contextBudgetObservation{}, false)
	if retry {
		t.Fatalf("unexpected one-shot recovery retry: %s", reason)
	}
	if decider.reasoningRetryErr != nil {
		t.Fatalf("unexpected reasoning retry error: %v", decider.reasoningRetryErr)
	}
	return decider.onDisposition(response, data, request.Body)
}

func TestRecoveryDeciderHeaderTimeoutAbsorbsAndRetries(t *testing.T) {
	absorb := stubDeciderAbsorb(config.Route{}, false)
	decider := newTestDecider(config.Route{}, absorb, context.Background(), nil)

	plan, reason := decider.onRequestError(errUpstreamResponseHeaderTimeout, []byte(deciderProbeBody))
	if plan != planRetryRequest {
		t.Fatalf("plan=%v, want planRetryRequest", plan)
	}
	if decider.absorbReason != "response header timeout" || decider.absorbStatus != 0 {
		t.Fatalf("absorb log fields=%q status=%d", decider.absorbReason, decider.absorbStatus)
	}
	if string(decider.retryBody) != deciderProbeBody {
		t.Fatalf("retry body=%q, want the current request body", decider.retryBody)
	}
	if reason != "" {
		t.Fatalf("absorb retry logs via logAbsorbWait, got reason=%q", reason)
	}
	if absorb.attempt != 1 {
		t.Fatalf("absorb attempts=%d, want 1", absorb.attempt)
	}
}

func TestRecoveryDeciderHeaderTimeoutWindowExhaustedWrites504(t *testing.T) {
	absorb := stubDeciderAbsorb(config.Route{}, true)
	decider := newTestDecider(config.Route{}, absorb, context.Background(), nil)

	plan, _ := decider.onRequestError(errUpstreamResponseHeaderTimeout, []byte(deciderProbeBody))
	if plan != planWriteError {
		t.Fatalf("plan=%v, want planWriteError", plan)
	}
	if decider.errStatus != http.StatusGatewayTimeout || !decider.errRetryable {
		t.Fatalf("error status=%d retryable=%t, want retryable 504", decider.errStatus, decider.errRetryable)
	}
	if decider.errType != "proxy_error" || decider.errMessage != "upstream timed out before returning response headers" {
		t.Fatalf("error type=%q message=%q", decider.errType, decider.errMessage)
	}
}

func TestRecoveryDeciderAbortedWaitStaysSilent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	absorb := stubDeciderAbsorb(config.Route{}, true)
	decider := newTestDecider(config.Route{}, absorb, ctx, nil)

	plan, reason := decider.onRequestError(errUpstreamResponseHeaderTimeout, []byte(deciderProbeBody))
	if plan != planAbortSilent {
		t.Fatalf("plan=%v, want planAbortSilent", plan)
	}
	if !strings.Contains(reason, "request abandoned after absorb wait") {
		t.Fatalf("reason=%q", reason)
	}
}

func TestRecoveryDeciderStopWindowWritesStructuredError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	absorb := stubDeciderAbsorb(config.Route{}, false)
	decider := newTestDecider(config.Route{}, absorb, ctx, func() bool { return false })

	plan, _ := decider.onRequestError(errors.New("dial tcp: connection refused"), []byte(deciderProbeBody))
	if plan != planWriteError {
		t.Fatalf("plan=%v, want planWriteError", plan)
	}
	if decider.errStatus != http.StatusServiceUnavailable || decider.errRetryable {
		t.Fatalf("error status=%d retryable=%t, want non-retryable 503", decider.errStatus, decider.errRetryable)
	}
	if decider.errType != "proxy_stopped" || decider.errMessage != "hellogrok 代理已停止，请重新选择模型" {
		t.Fatalf("error type=%q message=%q", decider.errType, decider.errMessage)
	}
}

func TestRecoveryDeciderDialFailureFlagsBreakerCount(t *testing.T) {
	decider := newTestDecider(config.Route{}, stubDeciderAbsorb(config.Route{}, false), context.Background(), nil)

	plan, _ := decider.onRequestError(&net.DNSError{Name: "dead.example", Err: "no such host"}, []byte(deciderProbeBody))
	if plan != planWriteError || !decider.dialFailure {
		t.Fatalf("plan=%v dialFailure=%t, want planWriteError with a dial-level failure", plan, decider.dialFailure)
	}
	if decider.errStatus != http.StatusBadGateway || !decider.errRetryable || !strings.HasPrefix(decider.errMessage, "upstream: ") {
		t.Fatalf("error status=%d retryable=%t message=%q", decider.errStatus, decider.errRetryable, decider.errMessage)
	}

	plan, _ = decider.onRequestError(errors.New("server closed idle connection"), []byte(deciderProbeBody))
	if plan != planWriteError || decider.dialFailure {
		t.Fatalf("plan=%v dialFailure=%t, want planWriteError without a dial-level failure", plan, decider.dialFailure)
	}
}

func TestRecoveryDeciderOffTier401PassesThroughWithoutRetry(t *testing.T) {
	absorb := stubDeciderAbsorb(config.Route{}, false)
	decider := newTestDecider(config.Route{}, absorb, context.Background(), nil)
	request := facadeRequest{Body: []byte(deciderProbeBody), Protocol: wireResponses, IncomingProtocol: wireResponses}
	response := newTestResponse(http.StatusUnauthorized, http.Header{})
	data := []byte(`{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"channel key rotated"}}`)

	plan, reason := decideErrorPath(t, decider, response, data, request)
	if plan != planPassThrough {
		t.Fatalf("plan=%v, want planPassThrough", plan)
	}
	if decider.status != http.StatusUnauthorized || decider.retryable {
		t.Fatalf("status=%d retryable=%t, want non-retryable 401", decider.status, decider.retryable)
	}
	if absorb.attempt != 0 {
		t.Fatalf("absorb attempts=%d, want 0", absorb.attempt)
	}
	if response.Header.Get("X-Should-Retry") != "false" {
		t.Fatalf("X-Should-Retry=%q, want false", response.Header.Get("X-Should-Retry"))
	}
	if reason != "" {
		t.Fatalf("plain passthrough must stay silent, got reason=%q", reason)
	}
}

func TestRecoveryDeciderBalancedTier401Absorbs(t *testing.T) {
	route := config.Route{ErrorResilience: "balanced"}
	absorb := stubDeciderAbsorb(route, false)
	decider := newTestDecider(route, absorb, context.Background(), nil)
	request := facadeRequest{Body: []byte(deciderProbeBody), Protocol: wireResponses, IncomingProtocol: wireResponses}
	response := newTestResponse(http.StatusUnauthorized, http.Header{})
	data := []byte(`{"error":{"type":"invalid_request_error","code":"invalid_api_key","message":"channel key rotated"}}`)

	plan, reason := decideErrorPath(t, decider, response, data, request)
	if plan != planRetryRequest {
		t.Fatalf("plan=%v, want planRetryRequest", plan)
	}
	if decider.absorbReason != "deterministic upstream error" || decider.absorbStatus != http.StatusUnauthorized {
		t.Fatalf("absorb log fields=%q status=%d", decider.absorbReason, decider.absorbStatus)
	}
	if string(decider.retryBody) != deciderProbeBody {
		t.Fatalf("retry body=%q, want the unchanged request body", decider.retryBody)
	}
	if absorb.attempt != 1 {
		t.Fatalf("absorb attempts=%d, want 1", absorb.attempt)
	}
	if reason != "" {
		t.Fatalf("absorb retry logs via logAbsorbWait, got reason=%q", reason)
	}
}

func TestRecoveryDeciderContextBudgetClampsOnce(t *testing.T) {
	absorb := stubDeciderAbsorb(config.Route{}, true)
	decider := newTestDecider(config.Route{}, absorb, context.Background(), nil)
	request := facadeRequest{
		Body:             []byte(`{"model":"display","input":"hi","max_output_tokens":384000,"stream":false}`),
		Protocol:         wireResponses,
		IncomingProtocol: wireResponses,
	}
	observation := contextBudgetObservation{MaximumTokens: 1048576, MessageTokens: 664712, CompletionTokens: 384000, ExactRequest: true}
	response := newTestResponse(http.StatusBadRequest, http.Header{})
	data := []byte(`{"error":{"message":"This model's maximum context length is 1048576 tokens. However, you requested 1048712 tokens (664712 in the messages, 384000 in the completion). Please reduce the length of the messages or completion.","type":"invalid_request_error"}}`)

	retry, reason := decider.onErrorResponse(response, data, request, observation, true)
	if !retry {
		t.Fatal("first exact budget error must trigger the clamp retry")
	}
	if !decider.contextBudgetRetried || decider.contextOutputLimit != 383864 {
		t.Fatalf("contextOutputLimit=%d retried=%t, want 383864 once", decider.contextOutputLimit, decider.contextBudgetRetried)
	}
	root, err := decodeRequestObject(decider.retryBody)
	if err != nil {
		t.Fatal(err)
	}
	if limit, _ := positiveJSONUint64(root["max_output_tokens"]); limit != 383864 {
		t.Fatalf("retry max_output_tokens=%d, want 383864", limit)
	}
	if !strings.Contains(reason, "context budget retry once") || !strings.Contains(reason, "completion_after=383864") {
		t.Fatalf("reason=%q", reason)
	}

	// The clamp is one-shot: a second budget error passes through instead.
	plan, reason := decideErrorPath(t, decider, response, data, request)
	if plan != planPassThrough {
		t.Fatalf("plan=%v, want planPassThrough after the one-shot retry", plan)
	}
	if decider.status != http.StatusBadRequest || decider.retryable {
		t.Fatalf("status=%d retryable=%t, want non-retryable 400", decider.status, decider.retryable)
	}
	if reason != "" {
		t.Fatalf("plain passthrough must stay silent, got reason=%q", reason)
	}
}

func TestRecoveryDeciderReasoningRejectedVetoesRetryAndAbsorb(t *testing.T) {
	route := config.Route{ErrorResilience: "balanced"}
	absorb := stubDeciderAbsorb(route, false)
	decider := newTestDecider(route, absorb, context.Background(), nil)
	// Every opaque block was already dropped, so no recovery rewrite is
	// possible; the rejection must still veto the absorb layer and the
	// retry disposition.
	request := facadeRequest{
		Body:             []byte(deciderProbeBody),
		Protocol:         wireResponses,
		IncomingProtocol: wireResponses,
		Reasoning:        reasoningFilterStats{Opaque: 2, Dropped: 2},
	}
	response := newTestResponse(http.StatusBadRequest, http.Header{})
	data := []byte(`{"error":{"type":"invalid_request_error","message":"encrypted_content belongs to another model family"}}`)

	plan, reason := decideErrorPath(t, decider, response, data, request)
	if plan != planPassThrough {
		t.Fatalf("plan=%v, want planPassThrough", plan)
	}
	if !decider.reasoningRejected {
		t.Fatal("reasoningRejected must be set")
	}
	if decider.retryable || decider.status != http.StatusBadRequest {
		t.Fatalf("status=%d retryable=%t, want non-retryable 400", decider.status, decider.retryable)
	}
	if response.Header.Get("X-Should-Retry") != "false" {
		t.Fatalf("X-Should-Retry=%q, want false", response.Header.Get("X-Should-Retry"))
	}
	if absorb.attempt != 0 {
		t.Fatalf("absorb attempts=%d, want 0 (the rejection vetoes even the balanced tier)", absorb.attempt)
	}
	if reason != "" {
		t.Fatalf("plain passthrough must stay silent, got reason=%q", reason)
	}
}

func TestRecoveryDeciderCloudflareChallengeRewritesToRetryable503(t *testing.T) {
	absorb := stubDeciderAbsorb(config.Route{}, true)
	decider := newTestDecider(config.Route{}, absorb, context.Background(), nil)
	request := facadeRequest{Body: []byte(deciderProbeBody), Protocol: wireResponses, IncomingProtocol: wireResponses}
	response := newTestResponse(http.StatusForbidden, http.Header{"Cf-Mitigated": {"challenge"}})
	data := []byte(`{"error":{"type":"server_error","message":"just a moment"}}`)

	plan, _ := decideErrorPath(t, decider, response, data, request)
	if plan != planPassThrough {
		t.Fatalf("plan=%v, want planPassThrough", plan)
	}
	if response.StatusCode != http.StatusServiceUnavailable || decider.status != http.StatusServiceUnavailable {
		t.Fatalf("status=%d decider.status=%d, want the 403 rewritten to 503", response.StatusCode, decider.status)
	}
	if !decider.retryable || response.Header.Get("X-Should-Retry") != "true" {
		t.Fatalf("retryable=%t X-Should-Retry=%q, want a retryable challenge", decider.retryable, response.Header.Get("X-Should-Retry"))
	}
}

func TestRecoveryDecider408PassesThroughAs504(t *testing.T) {
	absorb := stubDeciderAbsorb(config.Route{}, true)
	decider := newTestDecider(config.Route{}, absorb, context.Background(), nil)
	request := facadeRequest{Body: []byte(deciderProbeBody), Protocol: wireResponses, IncomingProtocol: wireResponses}
	response := newTestResponse(http.StatusRequestTimeout, http.Header{"Content-Type": {"application/json"}})
	data := []byte(`{"error":{"type":"server_error","message":"edge gateway timeout"}}`)

	plan, _ := decideErrorPath(t, decider, response, data, request)
	if plan != planPassThrough {
		t.Fatalf("plan=%v, want planPassThrough", plan)
	}
	if decider.status != http.StatusRequestTimeout || !decider.retryable {
		t.Fatalf("status=%d retryable=%t, want a retryable 408 decision", decider.status, decider.retryable)
	}

	recorder := httptest.NewRecorder()
	writeRecoveryPassthrough(recorder, response, data, decider, 0)
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("passthrough status=%d, want 504", recorder.Code)
	}
	if recorder.Header().Get("X-Should-Retry") != "true" {
		t.Fatalf("X-Should-Retry=%q, want true", recorder.Header().Get("X-Should-Retry"))
	}
	// The 408 decision stays below 500, so no Retry-After is synthesized.
	if recorder.Header().Get("Retry-After") != "" {
		t.Fatalf("Retry-After=%q, want none for a 408", recorder.Header().Get("Retry-After"))
	}
	if recorder.Body.String() != string(data) {
		t.Fatalf("passthrough body=%q, want the upstream body byte-for-byte", recorder.Body.String())
	}
}

func TestRecoveryDeciderWrapped2xxTransientAbsorbs(t *testing.T) {
	absorb := stubDeciderAbsorb(config.Route{}, false)
	decider := newTestDecider(config.Route{}, absorb, context.Background(), nil)
	response := newTestResponse(http.StatusOK, http.Header{})
	data := []byte(`{"error":{"type":"rate_limit","message":"rate limit exceeded"}}`)

	plan, reason := decider.onSuccessEnvelope(response, data, []byte(deciderProbeBody))
	if plan != planRetryRequest {
		t.Fatalf("plan=%v, want planRetryRequest", plan)
	}
	if decider.absorbReason != "wrapped 2xx upstream error" || decider.absorbStatus != http.StatusServiceUnavailable {
		t.Fatalf("absorb log fields=%q status=%d", decider.absorbReason, decider.absorbStatus)
	}
	if absorb.attempt != 1 {
		t.Fatalf("absorb attempts=%d, want 1", absorb.attempt)
	}
	if response.Header.Get("X-Should-Retry") != "true" {
		t.Fatalf("X-Should-Retry=%q, want true", response.Header.Get("X-Should-Retry"))
	}
	if reason != "" {
		t.Fatalf("absorb retry logs via logAbsorbWait, got reason=%q", reason)
	}
}

func TestRecoveryDeciderWrapped2xxDeterministicPassesThrough(t *testing.T) {
	absorb := stubDeciderAbsorb(config.Route{}, true)
	decider := newTestDecider(config.Route{}, absorb, context.Background(), nil)
	response := newTestResponse(http.StatusOK, http.Header{})
	data := []byte(`{"error":{"type":"invalid_request_error","message":"invalid model"}}`)

	plan, reason := decider.onSuccessEnvelope(response, data, []byte(deciderProbeBody))
	if plan != planPassThrough {
		t.Fatalf("plan=%v, want planPassThrough", plan)
	}
	if decider.status != http.StatusBadRequest || decider.retryable {
		t.Fatalf("status=%d retryable=%t, want non-retryable 400", decider.status, decider.retryable)
	}
	if response.Header.Get("X-Should-Retry") != "false" {
		t.Fatalf("X-Should-Retry=%q, want false", response.Header.Get("X-Should-Retry"))
	}
	if !strings.Contains(reason, "wrapped 2xx error surrogate=400 retryable=false") {
		t.Fatalf("reason=%q", reason)
	}
}

func TestRecoveryDeciderSuccessEnvelopeBreaksToResponse(t *testing.T) {
	decider := newTestDecider(config.Route{}, stubDeciderAbsorb(config.Route{}, false), context.Background(), nil)
	response := newTestResponse(http.StatusOK, http.Header{})
	data := []byte(`{"object":"response","output":[]}`)

	plan, _ := decider.onSuccessEnvelope(response, data, []byte(deciderProbeBody))
	if plan != planBreakToResponse {
		t.Fatalf("plan=%v, want planBreakToResponse", plan)
	}
}
