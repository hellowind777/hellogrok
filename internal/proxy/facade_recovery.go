package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/hellowind777/hellogrok/internal/config"
)

// recoveryPlan is the decision for one iteration of the forwardFacade main
// loop. The loop owns the mechanics (doRequest, body reads, capacity
// observation, logging) and only executes the plan a decider call returns.
type recoveryPlan int

const (
	planRetryRequest    recoveryPlan = iota // resend the request with the decider's retry body
	planPassThrough                         // leave the loop and pass the current response through
	planBreakToResponse                     // success or stream: leave the loop for response handling
	planAbortSilent                         // context canceled: return without writing anything
	planWriteError                          // end with the decider's status and typed error
)

// recoveryDecider owns the forwardFacade retry/recovery decisions. It is one
// per request; persistent fields survive the loop iterations, the rest are
// per-decision outputs filled by each decide call.
type recoveryDecider struct {
	route   config.Route
	absorb  *absorbState
	ctx     context.Context
	stopped func() bool
	// provenance feeds the one-shot reasoning-recovery request rewrite.
	provenance *reasoningProvenanceStore

	contextBudgetRetried bool
	contextOutputLimit   uint64
	reasoningRecovered   bool
	visionStripped       bool
	// originalBody is the client request body as received, before any
	// recovery rewrite; the reasoning recovery always adapts from it.
	originalBody []byte

	// Per-decision outputs, filled by each decide call.
	retryBody         []byte
	retryRequest      facadeRequest
	reasoningRetried  bool
	reasoningRetryErr error
	absorbReason      string
	absorbStatus      int
	status            int
	retryable         bool
	reasoningRejected bool
	dialFailure       bool
	errStatus         int
	errType           string
	errMessage        string
	errRetryable      bool
}

func newRecoveryDecider(route config.Route, absorb *absorbState, ctx context.Context, stopped func() bool, provenance *reasoningProvenanceStore, originalBody []byte) *recoveryDecider {
	return &recoveryDecider{
		route:        route,
		absorb:       absorb,
		ctx:          ctx,
		stopped:      stopped,
		provenance:   provenance,
		originalBody: originalBody,
	}
}

// reset clears the per-decision outputs before the next decide call. The
// persistent state (contextBudgetRetried, contextOutputLimit,
// reasoningRecovered) survives across loop iterations.
func (d *recoveryDecider) reset() {
	d.retryBody = nil
	d.retryRequest = facadeRequest{}
	d.reasoningRetried = false
	d.reasoningRetryErr = nil
	d.absorbReason = ""
	d.absorbStatus = 0
	d.status = 0
	d.retryable = false
	d.reasoningRejected = false
	d.dialFailure = false
	d.errStatus = 0
	d.errType = ""
	d.errMessage = ""
	d.errRetryable = false
}

// onRequestError decides the request-error path: the stop-window diagnostic,
// the response-header-timeout absorb retry, and the final gateway error. The
// caller has already logged the raw failure.
func (d *recoveryDecider) onRequestError(err error, requestBody []byte) (recoveryPlan, string) {
	d.reset()
	if d.ctx.Err() != nil && !d.stopped() {
		// The proxy was stopped while this request was in flight.
		// Report the same structured stop error as the request
		// entry path so stale sessions know to reselect a model.
		d.errStatus = http.StatusServiceUnavailable
		d.errType = "proxy_stopped"
		d.errMessage = "hellogrok 代理已停止，请重新选择模型"
		d.errRetryable = false
		return planWriteError, ""
	}
	if isUpstreamTimeout(err) {
		if d.absorb.wait(d.ctx, absorbFamilyFor(0, false, "response header timeout"), 0) {
			d.absorbReason = "response header timeout"
			d.absorbStatus = 0
			d.retryBody = requestBody
			return planRetryRequest, ""
		}
		if d.ctx.Err() != nil {
			// The stop lifecycle or the client ended the request
			// during the wait; nothing can be written anymore.
			return planAbortSilent, fmt.Sprintf("request abandoned after absorb wait: %v", d.ctx.Err())
		}
		d.errStatus = http.StatusGatewayTimeout
		d.errType = "proxy_error"
		d.errMessage = "upstream timed out before returning response headers"
		d.errRetryable = true
		return planWriteError, ""
	}
	d.dialFailure = isHardUpstreamError(err)
	d.errStatus = http.StatusBadGateway
	d.errType = "proxy_error"
	d.errMessage = "upstream: " + safeUpstreamError(err)
	d.errRetryable = true
	return planWriteError, ""
}

// onSuccessEnvelope decides a 2xx response whose buffered body was read: a
// wrapped error envelope takes the failure path, everything else breaks to
// response handling.
func (d *recoveryDecider) onSuccessEnvelope(response *http.Response, data []byte, requestBody []byte) (recoveryPlan, string) {
	d.reset()
	surrogate, retryable, wrapped := wrappedUpstreamError(data)
	if !wrapped {
		return planBreakToResponse, ""
	}
	// The relay reported a failure with a success status. Give
	// it the failure path: transient envelopes are absorbed
	// and retried here, deterministic ones pass through with
	// the provider's explanation instead of an opaque
	// envelope-validation rejection.
	response.Header.Set("X-Should-Retry", fmt.Sprintf("%t", retryable))
	// A reasoning-history rejection wrapped in a 2xx envelope vetoes the
	// absorb layer exactly like its plain-error counterpart: it reports a
	// foreign conversation state the user must see, and this path has no
	// recovery rewrite, so absorbing would only burn the window.
	wrappedReasoningRejected := isOpaqueReasoningRejection(surrogate, data)
	if !wrappedReasoningRejected && absorbEligible(surrogate, retryable, d.absorb.resilience) &&
		d.absorb.wait(d.ctx, absorbFamilyFor(surrogate, retryable, "wrapped 2xx upstream error"), retryAfterSeconds(response.Header)) {
		d.absorbReason = "wrapped 2xx upstream error"
		d.absorbStatus = surrogate
		d.retryBody = requestBody
		return planRetryRequest, ""
	}
	if d.ctx.Err() != nil {
		// The stop lifecycle or the client ended the request
		// during the wait; nothing can be written anymore.
		return planAbortSilent, fmt.Sprintf("request abandoned after absorb wait: %v", d.ctx.Err())
	}
	if wrappedReasoningRejected {
		d.reasoningRejected = true
		response.Header.Set("X-Should-Retry", "false")
		retryable = false
	}
	d.status = surrogate
	d.retryable = retryable
	return planPassThrough, fmt.Sprintf("wrapped 2xx error surrogate=%d retryable=%t", surrogate, retryable)
}

// onErrorResponse decides the one-shot request-rewrite recoveries on an
// upstream error response: the vision strip, the context-budget clamp and the
// opaque-reasoning drop. It returns true when the loop must resend with
// retryRequest (whole request, reasoning recovery) or retryBody (body-only,
// vision strip or context clamp). The caller has already observed the capacity
// evidence.
func (d *recoveryDecider) onErrorResponse(response *http.Response, data []byte, request facadeRequest, observation contextBudgetObservation, observedBudget bool) (bool, string) {
	d.reset()
	if !d.visionStripped && isVisionUnsupportedError(response.StatusCode, data) {
		if stripped, removed := stripVisionContent(request.Body, request.Protocol); removed > 0 {
			d.visionStripped = true
			d.retryBody = stripped
			return true, fmt.Sprintf("vision strip retry once removed=%d after status=%d", removed, response.StatusCode)
		}
	}
	if !d.contextBudgetRetried && observedBudget {
		if retry, ok := clampCompletionForContextError(observation, request.Body, request.Protocol); ok {
			d.contextBudgetRetried = true
			d.contextOutputLimit = retry.AvailableOutput
			d.retryBody = retry.Body
			return true, fmt.Sprintf("context budget retry once maximum=%d messages=%d completion_before=%d completion_after=%d",
				retry.MaximumTokens, retry.MessageTokens, retry.OriginalOutput, retry.AvailableOutput)
		}
	}

	reasoningRejected := isOpaqueReasoningRejection(response.StatusCode, data)
	d.reasoningRejected = reasoningRejected
	keptOpaqueReasoning := request.Reasoning.Opaque - request.Reasoning.Dropped
	if reasoningRejected && keptOpaqueReasoning > 0 && !d.reasoningRecovered {
		retryRequest, retryErr := adaptFacadeRequestWithReasoning(d.originalBody, d.route, request.IncomingProtocol, d.provenance, dropAllOpaqueReasoning)
		if retryErr == nil && retryRequest.Reasoning.Dropped > request.Reasoning.Dropped {
			if d.contextOutputLimit > 0 {
				adjusted, ok := setCompletionLimit(retryRequest.Body, retryRequest.Protocol, d.contextOutputLimit)
				if !ok {
					retryErr = fmt.Errorf("preserve context output limit")
				} else {
					retryRequest.Body = adjusted
				}
			}
		}
		if retryErr == nil && retryRequest.Reasoning.Dropped > request.Reasoning.Dropped {
			d.reasoningRecovered = true
			d.reasoningRetried = true
			d.retryRequest = retryRequest
			return true, fmt.Sprintf("reasoning recovery retry once removed=%d after status=%d", retryRequest.Reasoning.Dropped, response.StatusCode)
		}
		d.reasoningRetryErr = retryErr
	}
	return false, ""
}

// onDisposition decides the retry disposition of an upstream error response
// and whether the absorb layer may swallow it: retryable/absorbed failures
// resend the request, otherwise the failure passes through. It must run in
// the same iteration as onErrorResponse, whose reset prepared the outputs and
// whose reasoningRejected verdict it reuses.
func (d *recoveryDecider) onDisposition(response *http.Response, data []byte, requestBody []byte) (recoveryPlan, string) {
	d.retryBody = nil
	d.absorbReason = ""
	d.absorbStatus = 0
	d.status = 0
	d.retryable = false
	reasoningRejected := d.reasoningRejected

	// Decide the retry disposition on the upstream header first: the
	// absorb layer may swallow this failure, in which case nothing has
	// been written to the client yet and the loop re-sends the request.
	retryable := setRetryDisposition(response.Header, response.StatusCode, data)
	if reasoningRejected {
		response.Header.Set("X-Should-Retry", "false")
		retryable = false
	}
	if !reasoningRejected && isCloudflareChallenge(response.Header, data, response.StatusCode) {
		// A shield challenge in front of the relay is transient: it clears
		// or the relay routes around it. 403 would classify terminal in
		// Grok Build, so present the challenge as a retryable 503 and let
		// the absorb layer hide short challenges.
		if response.StatusCode == http.StatusForbidden {
			response.StatusCode = http.StatusServiceUnavailable
		}
		response.Header.Set("X-Should-Retry", "true")
		retryable = true
	}
	absorbReason := "retryable upstream error"
	if !retryable {
		if response.StatusCode == statusOriginTLSHandshake || response.StatusCode == statusOriginTLSCertificate {
			absorbReason = "origin-TLS upstream error"
		} else {
			absorbReason = "deterministic upstream error"
		}
	}
	// reasoningRejected vetoes the absorb layer even in the balanced
	// tier, which would otherwise swallow the deterministic 4xx: the
	// rejection tells the user their own reasoning history is foreign,
	// and hiding it behind the window would only delay the recovery.
	if !reasoningRejected && absorbEligible(response.StatusCode, retryable, d.absorb.resilience) &&
		d.absorb.wait(d.ctx, absorbFamilyFor(response.StatusCode, retryable, absorbReason), retryAfterSeconds(response.Header)) {
		d.absorbReason = absorbReason
		d.absorbStatus = response.StatusCode
		d.retryBody = requestBody
		return planRetryRequest, ""
	}
	if d.ctx.Err() != nil {
		// The stop lifecycle or the client ended the request during the
		// wait; nothing can be written anymore.
		return planAbortSilent, fmt.Sprintf("request abandoned after absorb wait: %v", d.ctx.Err())
	}
	d.status = response.StatusCode
	d.retryable = retryable
	return planPassThrough, ""
}

// writeRecoveryPassthrough assembles the client-facing failure passthrough
// for both passthrough plans (wrapped 2xx envelopes and plain error
// responses): header merge, 408→504 remap, Retry-After synthesis, and the
// absorb staleness annotations. It must run before any response bytes are
// written.
func writeRecoveryPassthrough(w http.ResponseWriter, response *http.Response, data []byte, decider *recoveryDecider, discoveredContextWindow uint64) {
	mergeGrokModelHeaders(w.Header(), response.Header)
	if !positiveModelHeader(w.Header().Get(grokContextWindowHeader), 64) && discoveredContextWindow > 0 {
		w.Header().Set(grokContextWindowHeader, fmt.Sprintf("%d", discoveredContextWindow))
	}
	copySafeResponseHeaders(w.Header(), response.Header)
	if decider.reasoningRejected {
		w.Header().Set("X-Should-Retry", "false")
	}
	if decider.retryable && decider.status >= http.StatusInternalServerError && retryAfterSeconds(w.Header()) == 0 {
		w.Header().Set("Retry-After", strconv.Itoa(synthesizedRetryAfterSeconds))
	}
	markAbsorbedFailureHeader(w.Header(), decider.absorb)
	w.WriteHeader(passthroughStatus(decider.status))
	_, _ = w.Write(markAbsorbedFailureBody(data, decider.absorb))
}
