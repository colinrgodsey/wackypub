package agent

import (
	"context"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// sequencedStubModel returns a different canned reply for each successive call, repeating
// the last entry once the sequence is exhausted. Used to simulate a backend that is
// degenerate for N attempts before (optionally) producing valid output, so a test can both
// drive the retry path deterministically and assert exactly how many attempts were made.
type sequencedStubModel struct {
	name    string
	replies []string
	calls   int32
}

func (m *sequencedStubModel) Name() string { return m.name }

func (m *sequencedStubModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		n := atomic.AddInt32(&m.calls, 1)
		idx := int(n) - 1
		if idx >= len(m.replies) {
			idx = len(m.replies) - 1
		}
		yield(&model.LLMResponse{
			Content: genai.NewContentFromText(m.replies[idx], "model"),
		}, nil)
	}
}

func (m *sequencedStubModel) callCount() int32 { return atomic.LoadInt32(&m.calls) }

// majorPassFixture builds a temp agent dir whose next compaction cycle is deterministically a
// MAJOR pass (compact-cycle: 2, major-compact-every: 3 -> next cycle 3, 3%3==0), with a known
// pre-existing MEMORY.md body so a test can assert it survives untouched on failure.
const degenerateTestOldMemoryBody = "# Task Overview\n\nPre-existing narrative that must survive a failed major pass untouched."

func majorPassFixture(t *testing.T) (tempDir string, cfg *RuntimeConfig, compactCfg *CompactConfig) {
	t.Helper()
	tempDir = t.TempDir()
	turns := []*genai.Content{
		genai.NewContentFromText("user one", "user"),
		genai.NewContentFromText("model one", "model"),
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if err := WriteMemoryFile(tempDir, "---\ncompact-cycle: 2\n---\n\n"+degenerateTestOldMemoryBody); err != nil {
		t.Fatalf("write memory: %v", err)
	}
	cfg = &RuntimeConfig{ContextWindow: 1000, Endpoint: "http://degenerate.test", Model: "degenerate-model"}
	compactCfg = &CompactConfig{CompactOverheadPct: 20, MajorCompactOverheadPct: 40, MajorCompactEvery: 3}
	return tempDir, cfg, compactCfg
}

// Acceptance (card major-compaction-pass-accepts-degenerate-output): degenerate major-pass
// output is rejected and retried, then falls back to preserving the old MEMORY.md with a
// logged error once retries are exhausted.
func TestCompactionDegenerateOutput_RejectedRetriedThenPreservesOldMemory(t *testing.T) {
	tempDir, cfg, compactCfg := majorPassFixture(t)
	stub := &sequencedStubModel{name: "stub", replies: []string{"ok"}} // always degenerate (2 chars)
	var denials int64
	a, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, cfg, stub, tempDir, nil, &denials)
	if err != nil {
		t.Fatalf("build compaction agent: %v", err)
	}

	compacted, err := CheckAndCompactSession(context.Background(), tempDir, cfg, a, true, compactCfg, &denials)
	if err == nil {
		t.Fatal("expected an error after exhausting retries on degenerate output, got nil")
	}
	if !strings.Contains(err.Error(), "degenerate") {
		t.Errorf("expected a degenerate-output error, got: %v", err)
	}
	if compacted {
		t.Error("expected compacted=false on a failed pass")
	}
	if got, want := stub.callCount(), int32(maxCompactionOutputRetries+1); got != want {
		t.Errorf("expected exactly %d generation attempts (maxCompactionOutputRetries+1), got %d", want, got)
	}

	mem, readErr := ReadMemoryFile(tempDir)
	if readErr != nil {
		t.Fatalf("read memory: %v", readErr)
	}
	if !strings.Contains(mem, degenerateTestOldMemoryBody) {
		t.Errorf("the pre-existing MEMORY.md must survive untouched on failure, got:\n%s", mem)
	}
}

// Acceptance: a backend that is degenerate for its first attempts but produces valid output
// within the retry budget succeeds, and the valid output is what gets persisted.
func TestCompactionDegenerateOutput_SucceedsWithinRetryBudget(t *testing.T) {
	tempDir, cfg, compactCfg := majorPassFixture(t)
	validSummary := "# Task Overview\n\nConsolidated after retrying past two degenerate replies."
	stub := &sequencedStubModel{name: "stub", replies: []string{"x", "y", validSummary}}
	var denials int64
	a, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, cfg, stub, tempDir, nil, &denials)
	if err != nil {
		t.Fatalf("build compaction agent: %v", err)
	}

	compacted, err := CheckAndCompactSession(context.Background(), tempDir, cfg, a, true, compactCfg, &denials)
	if err != nil {
		t.Fatalf("expected success after the retry budget covers the eventual valid reply, got: %v", err)
	}
	if !compacted {
		t.Fatal("expected compacted=true")
	}
	if got, want := stub.callCount(), int32(3); got != want {
		t.Errorf("expected exactly 3 attempts (2 degenerate + 1 valid), got %d", got)
	}

	mem, readErr := ReadMemoryFile(tempDir)
	if readErr != nil {
		t.Fatalf("read memory: %v", readErr)
	}
	if !strings.Contains(mem, "Consolidated after retrying past two degenerate replies.") {
		t.Errorf("expected the eventual valid summary to be persisted, got:\n%s", mem)
	}
	if strings.Contains(mem, degenerateTestOldMemoryBody) {
		t.Errorf("a successful major pass must replace the old narrative, got:\n%s", mem)
	}
}

// Acceptance: valid output persists unchanged behavior - no unnecessary retries, no change
// to the existing happy path.
func TestCompactionDegenerateOutput_ValidOutputUnaffected(t *testing.T) {
	tempDir, cfg, compactCfg := majorPassFixture(t)
	validSummary := "# Task Overview\n\nA perfectly ordinary consolidated summary, first try."
	stub := &sequencedStubModel{name: "stub", replies: []string{validSummary}}
	var denials int64
	a, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, cfg, stub, tempDir, nil, &denials)
	if err != nil {
		t.Fatalf("build compaction agent: %v", err)
	}

	compacted, err := CheckAndCompactSession(context.Background(), tempDir, cfg, a, true, compactCfg, &denials)
	if err != nil {
		t.Fatalf("unexpected error on a valid first-attempt reply: %v", err)
	}
	if !compacted {
		t.Fatal("expected compacted=true")
	}
	if got := stub.callCount(); got != 1 {
		t.Errorf("a valid first attempt must not be retried, got %d call(s)", got)
	}

	mem, readErr := ReadMemoryFile(tempDir)
	if readErr != nil {
		t.Fatalf("read memory: %v", readErr)
	}
	if !strings.Contains(mem, "A perfectly ordinary consolidated summary, first try.") {
		t.Errorf("expected the valid summary to be persisted, got:\n%s", mem)
	}
}

// Acceptance: a minor (append) pass keeps the light validation bar - a short bullet is
// completely legitimate output there, not a bug, and must not be rejected or retried.
func TestCompactionDegenerateOutput_MinorPassAcceptsShortBullet(t *testing.T) {
	tempDir := t.TempDir()
	turns := []*genai.Content{
		genai.NewContentFromText("user one", "user"),
		genai.NewContentFromText("model one", "model"),
	}
	if err := WriteSessionTurns(tempDir, turns); err != nil {
		t.Fatalf("write session: %v", err)
	}
	if err := WriteMemoryFile(tempDir, "# Task Overview\n\nExisting narrative."); err != nil {
		t.Fatalf("write memory: %v", err)
	}
	cfg := &RuntimeConfig{ContextWindow: 1000, Endpoint: "http://minor.test", Model: "minor-model"}
	compactCfg := &CompactConfig{AppendOnly: true, CompactOverheadPct: 20}
	stub := &sequencedStubModel{name: "stub", replies: []string{"* ok"}} // 4 chars - would fail the major-pass floor
	var denials int64
	a, err := BuildADKAgentWithConfigAndTrackerForCompaction("agent", "system", DefaultMaxToolTurns, cfg, stub, tempDir, nil, &denials)
	if err != nil {
		t.Fatalf("build compaction agent: %v", err)
	}

	compacted, err := CheckAndCompactSession(context.Background(), tempDir, cfg, a, true, compactCfg, &denials)
	if err != nil {
		t.Fatalf("a short minor-pass bullet must not be rejected: %v", err)
	}
	if !compacted {
		t.Fatal("expected compacted=true")
	}
	if got := stub.callCount(); got != 1 {
		t.Errorf("a valid (if short) minor-pass reply must not be retried, got %d call(s)", got)
	}

	mem, readErr := ReadMemoryFile(tempDir)
	if readErr != nil {
		t.Fatalf("read memory: %v", readErr)
	}
	if !strings.Contains(mem, "* ok") {
		t.Errorf("expected the short bullet to be appended, got:\n%s", mem)
	}
}

// Unit-level coverage for validateCompactionOutput itself, independent of the full
// compaction plumbing above.
func TestValidateCompactionOutput(t *testing.T) {
	cases := []struct {
		name    string
		output  string
		isMajor bool
		wantErr bool
	}{
		{"empty minor rejected", "", false, true},
		{"empty major rejected", "", true, true},
		{"short minor accepted", "* ok", false, false},
		{"short major rejected", "ok", true, true},
		{"long major accepted", strings.Repeat("real consolidated content ", 5), true, false},
		{"refusal major rejected", "I cannot summarize this conversation for you.", true, true},
		{"refusal-looking text is fine for minor", "I cannot believe how much got done today", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCompactionOutput(tc.output, tc.isMajor)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateCompactionOutput(%q, isMajor=%v) error = %v, wantErr %v", tc.output, tc.isMajor, err, tc.wantErr)
			}
		})
	}
}
