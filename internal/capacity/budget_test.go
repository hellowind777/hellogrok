package capacity

import (
	"math"
	"testing"
)

func TestCalculateAutoCompactBudget(t *testing.T) {
	tests := []struct {
		name         string
		window       uint64
		completion   uint64
		desired      uint8
		wantMargin   uint64
		wantSafe     uint8
		want         uint8
		wantReady    bool
		wantConflict bool
	}{
		{
			name: "one million context and 384K output", window: 1_048_576,
			completion: 384_000, desired: 85, wantMargin: 52_429,
			wantSafe: 58, want: 58, wantReady: true,
		},
		{
			name: "384K context", window: 393_216,
			completion: 65_536, desired: 85, wantMargin: 19_661,
			wantSafe: 78, want: 78, wantReady: true,
		},
		{
			name: "lower user threshold is preserved", window: 1_048_576,
			completion: 384_000, desired: 50, wantMargin: 52_429,
			wantSafe: 58, want: 50, wantReady: true,
		},
		{
			name: "missing context", completion: 8_192, desired: 85,
		},
		{
			name: "missing completion", window: 131_072, desired: 85,
		},
		{
			name: "reserve consumes context", window: 16_384,
			completion: 8_192, desired: 85, wantMargin: 8_192,
			wantConflict: true,
		},
		{
			name: "uint64 boundary", window: math.MaxUint64,
			completion: 1, desired: 100, wantMargin: 922_337_203_685_477_581,
			wantSafe: 94, want: 94, wantReady: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Calculate(test.window, test.completion, test.desired)
			if got.Margin != test.wantMargin || got.SafeThreshold != test.wantSafe ||
				got.EffectiveThreshold != test.want || got.Ready != test.wantReady ||
				got.Conflict != test.wantConflict {
				t.Fatalf("Calculate() = %+v", got)
			}
		})
	}
}

func TestCalculateDoesNotGenerateZeroThreshold(t *testing.T) {
	got := Calculate(1_000_000, 982_000, 85)
	if got.Ready || !got.Conflict || got.SafeThreshold != 0 {
		t.Fatalf("near-exhausted capacity must be reported as a conflict: %+v", got)
	}
}
