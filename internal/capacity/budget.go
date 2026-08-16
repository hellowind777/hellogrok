package capacity

import "math/bits"

const (
	minimumSafetyMargin uint64 = 8_192
	safetyMarginDivisor uint64 = 20
)

// Budget is a model-specific auto-compaction decision. Ready is false until
// both capacity values are known. Conflict means no positive percentage can
// preserve the completion reserve and safety margin.
type Budget struct {
	ContextWindow       uint64
	MaxCompletionTokens uint64
	Margin              uint64
	DesiredThreshold    uint8
	SafeThreshold       uint8
	EffectiveThreshold  uint8
	Ready               bool
	Conflict            bool
}

// Calculate reserves the configured output budget plus five percent of the
// context window, with an 8K minimum for token-estimation and summary overhead.
// It only lowers an unsafe desired threshold.
func Calculate(contextWindow, maxCompletionTokens uint64, desiredThreshold uint8) Budget {
	budget := Budget{
		ContextWindow: contextWindow, MaxCompletionTokens: maxCompletionTokens,
		DesiredThreshold: desiredThreshold,
	}
	if contextWindow == 0 || maxCompletionTokens == 0 {
		return budget
	}
	margin := contextWindow / safetyMarginDivisor
	if contextWindow%safetyMarginDivisor != 0 {
		margin++
	}
	if margin < minimumSafetyMargin {
		margin = minimumSafetyMargin
	}
	budget.Margin = margin
	if maxCompletionTokens >= contextWindow || margin >= contextWindow-maxCompletionTokens {
		budget.Conflict = true
		return budget
	}
	available := contextWindow - maxCompletionTokens - margin
	high, low := bits.Mul64(available, 100)
	percent, _ := bits.Div64(high, low, contextWindow)
	if percent == 0 {
		budget.Conflict = true
		return budget
	}
	if percent > 100 {
		percent = 100
	}
	budget.SafeThreshold = uint8(percent)
	budget.EffectiveThreshold = desiredThreshold
	if budget.EffectiveThreshold > budget.SafeThreshold {
		budget.EffectiveThreshold = budget.SafeThreshold
	}
	budget.Ready = true
	return budget
}
