package agent

import (
	"math"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"
)

// Pins for the truthful-threshold work
// (tasks/wackypub/compaction-warning-false-positive, items 3, 5 and 1).

// ---------- item 3: the ceiling is an explicit reply reserve, not a flat share ----------

func TestContextBudget_ExplicitReserveAndMargin(t *testing.T) {
	got := contextBudget(500000, 12000, 20)
	want := 500000 - 12000 - DefaultCompactionSafetyMarginTokens
	if got != want {
		t.Fatalf("budget = %d, want %d", got, want)
	}
	oldCeiling := int(float64(500000) * 0.8)
	if got <= oldCeiling {
		t.Fatalf("reserve-based budget %d should sit above the old flat-share ceiling %d", got, oldCeiling)
	}
}

func TestContextBudget_UnsetReserveUsesDefault(t *testing.T) {
	if contextBudget(500000, 0, 20) != contextBudget(500000, DefaultMaxOutputReserveTokens, 20) {
		t.Fatal("an unset reserve should resolve to DefaultMaxOutputReserveTokens")
	}
}

func TestContextBudget_CustomReserveHonoured(t *testing.T) {
	got := contextBudget(100000, 40000, 20)
	want := 100000 - 40000 - DefaultCompactionSafetyMarginTokens
	if got != want {
		t.Fatalf("budget = %d, want %d", got, want)
	}
}

func TestContextBudget_FallsBackToPercentageWhenReserveExceedsWindow(t *testing.T) {
	// A 12k reserve cannot apply to a 50-token window; falling back keeps the stop
	// meaningful instead of disabling it forever.
	if got := contextBudget(50, 0, 20); got != 40 {
		t.Fatalf("fallback budget = %d, want 40", got)
	}
	if got := contextBudget(50, 0, 30); got != 35 {
		t.Fatalf("fallback budget at 30%% overhead = %d, want 35", got)
	}
}

func TestContextBudget_NonPositiveWindowIsZero(t *testing.T) {
	if got := contextBudget(0, 1000, 20); got != 0 {
		t.Fatalf("zero window budget = %d, want 0", got)
	}
	if got := contextBudget(-5, 1000, 20); got != 0 {
		t.Fatalf("negative window budget = %d, want 0", got)
	}
}

// ---------- item 1: one measure that says where the number came from ----------

func richTurns() []*genai.Content {
	return []*genai.Content{genai.NewContentFromText(strings.Repeat("a", 4000), "user")}
}

func TestContextUsed_PrefersProviderAndNamesIt(t *testing.T) {
	used, source := contextUsed(12345, richTurns(), false)
	if used != 12345 {
		t.Fatalf("used = %d, want the provider number 12345", used)
	}
	if source != UsageSourceProvider {
		t.Fatalf("source = %q, want %q", source, UsageSourceProvider)
	}
}

func TestContextUsed_ColdPathCalibratesTheEstimateExactlyOnce(t *testing.T) {
	turns := richTurns()
	est := int64(EstimateTokens(turns, false))
	used, source := contextUsed(0, turns, false)
	want := int64(math.Round(float64(est) * EstimateCalibrationFactor))
	if used != want {
		t.Fatalf("used = %d, want %d (raw estimate %d)", used, want, est)
	}
	if source != UsageSourceEstimate {
		t.Fatalf("source = %q, want %q", source, UsageSourceEstimate)
	}
	if used <= est {
		t.Fatalf("calibration must lift the raw estimate %d, got %d", est, used)
	}
}

// ---------- item 5: compaction stops destroying the deciding numbers ----------

func TestInvalidateLastUsage_PreservesTheDecidingNumbers(t *testing.T) {
	dir := t.TempDir()
	seed := &LastUsageRecord{
		PromptTokens:     431000,
		CandidatesTokens: 2200,
		TotalTokens:      433200,
		Timestamp:        time.Now(),
	}
	if err := WriteLastUsage(dir, seed); err != nil {
		t.Fatalf("WriteLastUsage: %v", err)
	}
	if err := InvalidateLastUsage(dir); err != nil {
		t.Fatalf("InvalidateLastUsage: %v", err)
	}
	rec, err := ReadLastUsage(dir)
	if err != nil {
		t.Fatalf("ReadLastUsage: %v", err)
	}
	if !rec.Compacted {
		t.Fatal("record should be marked compacted")
	}
	if rec.PromptTokens != 431000 || rec.CandidatesTokens != 2200 || rec.TotalTokens != 433200 {
		t.Fatalf("deciding numbers lost: %+v", rec)
	}
}

func TestInvalidateLastUsage_WithoutPriorRecordStillMarksCompacted(t *testing.T) {
	dir := t.TempDir()
	if err := InvalidateLastUsage(dir); err != nil {
		t.Fatalf("InvalidateLastUsage: %v", err)
	}
	rec, err := ReadLastUsage(dir)
	if err != nil {
		t.Fatalf("ReadLastUsage: %v", err)
	}
	if !rec.Compacted {
		t.Fatal("expected compacted marker with no prior record")
	}
}

func TestExceedsBudget_ZeroBudgetMeansNoHeadroom(t *testing.T) {
	// A window small enough that the ceiling truncates away must still trigger,
	// which is how a ContextWindow: 1 fixture forces compaction.
	if !exceedsBudget(0, contextBudget(1, 0, 20)) {
		t.Fatal("a zero budget should leave nothing unspent")
	}
	if exceedsBudget(0, 100) {
		t.Fatal("zero usage is not over a real budget")
	}
	if exceedsBudget(99, 100) {
		t.Fatal("99 is not over 100")
	}
	if !exceedsBudget(100, 100) {
		t.Fatal("100 is over a 100 budget")
	}
}

func TestEstimateCalibrationFactorCoversTheMeasuredBand(t *testing.T) {
	// The band is the 2026-09-19 measurement of how far EstimateTokens under-reads
	// provider prompt tokens. Pinned at the values measured so that editing the band
	// down to justify a lower factor fails here instead of in production.
	if MeasuredEstimatorDriftLow != 1.35 || MeasuredEstimatorDriftHigh != 1.65 {
		t.Fatalf("measured drift band changed: got %v-%v, want 1.35-1.65",
			MeasuredEstimatorDriftLow, MeasuredEstimatorDriftHigh)
	}
	if EstimateCalibrationFactor < MeasuredEstimatorDriftHigh {
		t.Fatalf("calibration factor %v is below the worst drift ever measured (%v), so the stop can fire after the window is gone",
			EstimateCalibrationFactor, MeasuredEstimatorDriftHigh)
	}
}

func TestEstimateCalibrationKeepsRealUsageInsideTheWindow(t *testing.T) {
	// Worst case on a large window: a session whose estimate sits just under the
	// stop, whose true drift is at the top of the measured band, and whose real
	// usage therefore must still be inside the window rather than rejected.
	const window = 200000
	budget := contextBudget(window, DefaultMaxOutputReserveTokens, 20)
	if exceedsBudget(int64(budget-1), budget) {
		t.Fatal("one below the budget must not trip the stop")
	}

	quietest := int64(math.Ceil(float64(budget)/EstimateCalibrationFactor)) - 1
	if exceedsBudget(quietest, budget) {
		t.Fatalf("the quietest estimate (%d) must not trip a %d budget", quietest, budget)
	}
	if realTop := float64(quietest) * MeasuredEstimatorDriftHigh; realTop >= float64(window) {
		t.Fatalf("at drift %v a session reaches %.0f provider-real tokens against a %d window",
			MeasuredEstimatorDriftHigh, realTop, window)
	}

	// The guard has to bite. The midpoint that stood here before fails the same
	// arithmetic, which is exactly why the factor moved to the band's upper bound.
	midpoint := 1.5
	midQuiet := int64(math.Ceil(float64(budget)/midpoint)) - 1
	if midReal := float64(midQuiet) * MeasuredEstimatorDriftHigh; midReal < float64(window) {
		t.Fatalf("expected the %v midpoint to overshoot a %d window at drift %v, but it computed %.0f",
			midpoint, window, MeasuredEstimatorDriftHigh, midReal)
	}
}
