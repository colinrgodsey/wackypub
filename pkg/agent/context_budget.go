package agent

import (
	"math"

	"google.golang.org/genai"
)

// Budget vocabulary for the context-window decisions (card
// tasks/wackypub/compaction-warning-false-positive).
//
// Before this, four sites each recomputed a ceiling as a flat share of the window
// and compared it against a different quantity: the mid-turn stop compared
// provider prompt tokens, post-turn compaction compared provider TOTAL tokens,
// and the cold-start guard and CheckAndCompactSession's own gate compared a raw
// chars/4 estimate. The stop could therefore fire where compaction saw nothing to
// do, which is the "warned but nothing shrank" report.
//
// Two functions own it now: contextBudget answers how much a turn may occupy, and
// contextUsed answers how much it occupies and measured how. Every decision site
// uses both, so a stop and the compaction it triggers cannot speak different units.
const (
	// DefaultMaxOutputReserveTokens is the reply headroom the mid-turn stop keeps
	// free. It replaces the habit of reserving 20-30% of the window, which on a
	// 500k window held roughly 150k tokens aside for a reply of a few thousand.
	DefaultMaxOutputReserveTokens = 12000

	// DefaultCompactionSafetyMarginTokens absorbs estimator jitter near the ceiling.
	DefaultCompactionSafetyMarginTokens = 2048

	// MeasuredEstimatorDriftLow and MeasuredEstimatorDriftHigh bracket how far
	// EstimateTokens under-reads provider-reported prompt tokens, measured on live
	// agent sessions on 2026-09-19. These are measurements, not tunables: the
	// calibration factor is derived from the top of the band.
	MeasuredEstimatorDriftLow  = 1.35
	MeasuredEstimatorDriftHigh = 1.65

	// EstimateCalibrationFactor corrects EstimateTokens for the drift above, and is
	// set to the top of the band rather than its midpoint because the two failure
	// directions are not equal: stopping early costs one round of latency, while
	// stopping late means the provider rejects the request outright. A factor has to
	// cover the worst case ever measured, not the average one.
	//
	// The midpoint fails that test. On a 200k window with a 12k reserve and the 2048
	// margin the budget is 185,952, and at 1.5 the stop would fire only once a
	// 1.65x session had reached 204,547 provider-real tokens, which is 4,547 past
	// the window. A fixed safety margin cannot absorb a ten percent residual at that
	// scale, so the factor carries the band instead.
	EstimateCalibrationFactor = MeasuredEstimatorDriftHigh

	// UsageSourceProvider marks a count reported by the model provider.
	UsageSourceProvider = "provider"
	// UsageSourceEstimate marks a count derived from EstimateTokens and calibrated.
	UsageSourceEstimate = "estimate"
)

// contextUsed returns the tokens occupying the model context plus the source of
// that number, so callers and user-visible messages can tell a real reading from
// an inference. providerTokens is the freshest provider-reported prompt count, or
// zero when nothing fresh exists: a cold process, or a usage record invalidated by
// compaction. turns is the body to estimate from in that case.
func contextUsed(providerTokens int64, turns []*genai.Content, preserveThinking bool) (int64, string) {
	if providerTokens > 0 {
		return providerTokens, UsageSourceProvider
	}
	estimated := EstimateTokens(turns, preserveThinking)
	return int64(math.Round(float64(estimated) * EstimateCalibrationFactor)), UsageSourceEstimate
}

// contextBudget is the token ceiling a turn may occupy before it must stop and let
// compaction run: the window less the reply reserve less a safety margin.
//
// When that leaves nothing positive, which happens for windows smaller than the
// reserve, it falls back to the percentage policy so small windows keep a
// meaningful ceiling instead of a stop that can never fire.
func contextBudget(contextWindow int, reserveTokens int, overheadPct float64) int {
	if contextWindow <= 0 {
		return 0
	}
	reserve := reserveTokens
	if reserve <= 0 {
		reserve = DefaultMaxOutputReserveTokens
	}
	if budget := contextWindow - reserve - DefaultCompactionSafetyMarginTokens; budget > 0 {
		return budget
	}
	if overheadPct < 0 || overheadPct >= 100 {
		overheadPct = DefaultCompactionOverheadPct
	}
	return int(float64(contextWindow) * (1.0 - (overheadPct / 100.0)))
}

// resolveOverheadPct reads the agent's COMPACT.md overhead setting, falling back
// to DefaultCompactionOverheadPct when unset or out of range.
func resolveOverheadPct(agentDir string) float64 {
	if cfg, err := LoadCompactConfig(agentDir); err == nil && cfg != nil {
		if cfg.CompactOverheadPct >= 0 && cfg.CompactOverheadPct < 100 {
			return cfg.CompactOverheadPct
		}
	}
	return DefaultCompactionOverheadPct
}

// exceedsBudget is the stop and compact test. A budget that truncated to zero, which
// is what a toy-sized window produces, means there is no headroom at all, so any
// usage is over it. Only a non-positive window disables the decision, and every
// caller guards on that before asking.
func exceedsBudget(used int64, budget int) bool {
	return used >= int64(budget)
}
