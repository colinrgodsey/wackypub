package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/genai"
	"google.golang.org/grpc"
)

// TestSeqMonotonicityConcurrentWriters verifies that sequence numbers assigned under
// the session lock across concurrent writers are strictly monotonically increasing
// with no duplicates and no gaps.
func TestSeqMonotonicityConcurrentWriters(t *testing.T) {
	t.Setenv("WACKYPUB_ALLOWED_AGENTS", "*")
	tempDir := t.TempDir()
	agentID := "concurrent-agent"
	agentDir := filepath.Join(tempDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	numWriters := 8
	writesPerWriter := 10
	totalWrites := numWriters * writesPerWriter

	var wg sync.WaitGroup
	wg.Add(numWriters)

	for w := 0; w < numWriters; w++ {
		go func(writerID int) {
			defer wg.Done()
			for i := 0; i < writesPerWriter; i++ {
				lock, err := AcquireSessionLockContext(context.Background(), agentDir)
				if err != nil {
					t.Errorf("acquire lock: %v", err)
					return
				}

				if (writerID+i)%2 == 0 {
					// Turn write
					turnText := fmt.Sprintf("message from writer %d iteration %d", writerID, i)
					_ = AppendSessionTurn(agentDir, "user", turnText)
				} else {
					// Tool event write
					sink := NewToolEventSinkWithJournal(toolJournalPath(agentDir))
					sink.SetSeqAlloc(func() int64 {
						seq, _ := NextSeq(agentDir)
						return seq
					})
					callID := sink.Announce("bash", "echo hello", false)
					sink.Update(callID, "bash", "completed", 5, "hello", "")
				}

				lock.Release()
			}
		}(w)
	}

	wg.Wait()

	events, baselineSeq, latestSeq, _, err := ReadSessionEventsFromDisk(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionEventsFromDisk: %v", err)
	}

	if len(events) == 0 {
		t.Fatalf("expected events, got 0")
	}

	// Verify strict monotonicity and no duplicates
	seenSeqs := make(map[int64]bool)
	var lastSeq int64
	for i, ev := range events {
		if ev.Seq <= 0 {
			t.Fatalf("event %d has invalid seq %d", i, ev.Seq)
		}
		if seenSeqs[ev.Seq] {
			t.Fatalf("duplicate sequence number %d found at event %d", ev.Seq, i)
		}
		seenSeqs[ev.Seq] = true
		if ev.Seq <= lastSeq {
			t.Fatalf("sequence decreased or stalled: last=%d, current=%d", lastSeq, ev.Seq)
		}
		lastSeq = ev.Seq
	}

	if baselineSeq != 1 {
		t.Errorf("expected baselineSeq 1, got %d", baselineSeq)
	}
	if latestSeq != lastSeq {
		t.Errorf("expected latestSeq %d, got %d", lastSeq, latestSeq)
	}
	if int(latestSeq) < totalWrites {
		t.Errorf("expected latestSeq >= %d, got %d", totalWrites, latestSeq)
	}
}

// TestCompactionRewindSignalling verifies that compaction replaces turns with a summary,
// assigns the summary a fresh high sequence number, preserves remaining turn sequence numbers,
// and signals rewound=true to consumers whose cursor was pruned.
func TestCompactionRewindSignalling(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	agentID := "compact-rewind-agent"
	agentDir := filepath.Join(tempDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write 10 turns (seq 1 to 10)
	for i := 1; i <= 10; i++ {
		if err := AppendSessionTurn(agentDir, "user", fmt.Sprintf("Turn %d", i)); err != nil {
			t.Fatalf("AppendSessionTurn %d: %v", i, err)
		}
	}

	// Add tool journal rows at seq 2 and seq 4 (pre-compaction turns)
	journalPath := toolJournalPath(agentDir)
	j1, _ := json.Marshal(ToolEvent{CallID: "c1", ToolName: "tool1", Seq: 2, Timestamp: time.Now()})
	j2, _ := json.Marshal(ToolEvent{CallID: "c2", ToolName: "tool2", Seq: 4, Timestamp: time.Now()})
	if err := os.WriteFile(journalPath, []byte(string(j1)+"\n"+string(j2)+"\n"), 0644); err != nil {
		t.Fatalf("write journal: %v", err)
	}

	sdk := NewSDK(tempDir)

	// Poll before compaction with since_seq: 3
	resp, err := sdk.ReadSessionEvents(context.Background(), &agentv1.ReadSessionEventsRequest{
		AgentId:      agentID,
		WorkspaceDir: tempDir,
		Cursor:       &agentv1.ReadSessionEventsRequest_SinceSeq{SinceSeq: 3},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents before compact: %v", err)
	}
	if resp.Rewound {
		t.Errorf("expected rewound=false before compaction")
	}

	// Compact first 5 turns
	// Read existing turns, compact turns 0..4, remaining turns are 5..9
	existingTurns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns: %v", err)
	}
	remainingTurns := existingTurns[5:] // turns 6 to 10

	notice := "Turns 1 to 5 archived into memory"
	noticeTurn := genai.NewContentFromText(FormatCompactionNotice(notice), "user")
	summarySeq, err := NextSeq(agentDir) // seq 11
	if err != nil {
		t.Fatalf("NextSeq: %v", err)
	}
	if summarySeq <= 10 {
		t.Fatalf("expected summarySeq > 10, got %d", summarySeq)
	}

	// Surviving turns kept their seqs: 6, 7, 8, 9, 10
	var pTurns []PersistedTurn
	pTurns = append(pTurns, PersistedTurn{Role: "user", Parts: noticeTurn.Parts, Seq: summarySeq})
	for i, t := range remainingTurns {
		pTurns = append(pTurns, PersistedTurn{Role: t.Role, Parts: t.Parts, Seq: int64(6 + i)})
	}
	if err := WritePersistedTurns(agentDir, pTurns); err != nil {
		t.Fatalf("WritePersistedTurns: %v", err)
	}
	// Verify that even if journal rows at 2 and 4 linger on disk (tool-journal.jsonl is an append-only sidecar),
	// the rewind baseline comes from the session stream (6), NOT the journal (2).
	// Now poll with since_seq: 3 (which was compacted away!)
	// Earliest surviving seq is 6. 3 < 6, so client was rewound!
	respAfter, err := sdk.ReadSessionEvents(context.Background(), &agentv1.ReadSessionEventsRequest{
		AgentId:      agentID,
		WorkspaceDir: tempDir,
		Cursor:       &agentv1.ReadSessionEventsRequest_SinceSeq{SinceSeq: 3},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents after compact: %v", err)
	}

	if respAfter.BaselineSeq != 6 {
		t.Fatalf("expected BaselineSeq 6 from session stream, got %d (dragged down by journal rows)", respAfter.BaselineSeq)
	}

	if !respAfter.Rewound {
		t.Fatalf("expected rewound=true when cursor 3 is below baseline 6")
	}
	// Baseline must be 6 (from session baseline), NOT 2 or 4 from old journal rows!
	if respAfter.BaselineSeq != 6 {
		t.Fatalf("expected baseline_seq=6, got %d", respAfter.BaselineSeq)
	}

	// Poll with since_seq: 7 (which survived!)
	// 7 >= 6, so not rewound
	respIntact, err := sdk.ReadSessionEvents(context.Background(), &agentv1.ReadSessionEventsRequest{
		AgentId:      agentID,
		WorkspaceDir: tempDir,
		Cursor:       &agentv1.ReadSessionEventsRequest_SinceSeq{SinceSeq: 7},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents for intact cursor: %v", err)
	}
	if respIntact.Rewound {
		t.Errorf("expected rewound=false for intact cursor 7 >= baseline 6")
	}
	// Events should be: seq 8, 9, 10, 11 (the compaction summary)
	if len(respIntact.Events) != 4 {
		t.Errorf("expected 4 events (8, 9, 10, 11), got %d", len(respIntact.Events))
	}
	lastEv := respIntact.Events[len(respIntact.Events)-1]
	if lastEv.Seq != summarySeq {
		t.Errorf("expected last event seq %d, got %d", summarySeq, lastEv.Seq)
	}
	if lastEv.GetCompaction() == nil {
		t.Errorf("expected compaction event at seq %d", summarySeq)
	}
}

// TestPollPathResumingFromCursor verifies that one-shot polling returns batches of events
// and cleanly resumes from the client's cursor without re-reading past events.
func TestPollPathResumingFromCursor(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	agentID := "poll-cursor-agent"
	agentDir := filepath.Join(tempDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	sdk := NewSDK(tempDir)

	_ = AppendSessionTurn(agentDir, "user", "Hello 1")
	_ = AppendSessionTurn(agentDir, "model", "Response 2")
	_ = AppendSessionTurn(agentDir, "user", "Hello 3")

	// First poll: read all
	resp1, err := sdk.ReadSessionEvents(context.Background(), &agentv1.ReadSessionEventsRequest{
		AgentId:      agentID,
		WorkspaceDir: tempDir,
	})
	if err != nil {
		t.Fatalf("poll 1: %v", err)
	}
	if len(resp1.Events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(resp1.Events))
	}
	cursor := resp1.Events[len(resp1.Events)-1].Seq
	if cursor != 3 {
		t.Fatalf("expected cursor 3, got %d", cursor)
	}

	// Second poll with cursor=3: empty batch
	resp2, err := sdk.ReadSessionEvents(context.Background(), &agentv1.ReadSessionEventsRequest{
		AgentId:      agentID,
		WorkspaceDir: tempDir,
		Cursor:       &agentv1.ReadSessionEventsRequest_SinceSeq{SinceSeq: cursor},
	})
	if err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	if len(resp2.Events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(resp2.Events))
	}

	// Write new turn and new tool event
	_ = AppendSessionTurn(agentDir, "user", "Hello 4")
	sink := NewToolEventSinkWithJournal(toolJournalPath(agentDir))
	sink.SetSeqAlloc(func() int64 {
		seq, _ := NextSeq(agentDir)
		return seq
	})
	callID := sink.Announce("cat", "file.txt", false)
	sink.Update(callID, "cat", "completed", 12, "file contents", "")

	// Third poll with cursor=3: returns events 4, 5, 6
	resp3, err := sdk.ReadSessionEvents(context.Background(), &agentv1.ReadSessionEventsRequest{
		AgentId:      agentID,
		WorkspaceDir: tempDir,
		Cursor:       &agentv1.ReadSessionEventsRequest_SinceSeq{SinceSeq: cursor},
	})
	if err != nil {
		t.Fatalf("poll 3: %v", err)
	}
	if len(resp3.Events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(resp3.Events))
	}
	if resp3.Events[0].Seq != 4 || resp3.Events[1].Seq != 5 || resp3.Events[2].Seq != 6 {
		t.Fatalf("unexpected seqs: %d, %d, %d", resp3.Events[0].Seq, resp3.Events[1].Seq, resp3.Events[2].Seq)
	}
}

// TestWatchRawByteIdentical verifies that event.Raw is byte-identical to the JSONL lines
// in session.jsonl and tool-journal.jsonl.
func TestWatchRawByteIdentical(t *testing.T) {
	tempDir := t.TempDir()
	agentID := "raw-byte-agent"
	agentDir := filepath.Join(tempDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	_ = AppendSessionTurn(agentDir, "user", "First raw turn")
	_ = AppendSessionTurn(agentDir, "model", "Second raw turn")

	sink := NewToolEventSinkWithJournal(toolJournalPath(agentDir))
	sink.SetSeqAlloc(func() int64 {
		seq, _ := NextSeq(agentDir)
		return seq
	})
	callID := sink.Announce("grep", "pattern file", false)
	sink.Update(callID, "grep", "completed", 20, "matched pattern", "")

	// Read raw lines from session.jsonl
	sessBytes, err := os.ReadFile(filepath.Join(agentDir, SessionFileName))
	if err != nil {
		t.Fatalf("read session.jsonl: %v", err)
	}
	sessLines := strings.Split(strings.TrimSpace(string(sessBytes)), "\n")

	// Read raw lines from tool-journal.jsonl
	journalBytes, err := os.ReadFile(toolJournalPath(agentDir))
	if err != nil {
		t.Fatalf("read tool-journal.jsonl: %v", err)
	}
	journalLines := strings.Split(strings.TrimSpace(string(journalBytes)), "\n")

	events, _, _, _, err := ReadSessionEventsFromDisk(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionEventsFromDisk: %v", err)
	}

	// Verify each raw string matches the corresponding file line exactly
	if events[0].Raw != sessLines[0] {
		t.Errorf("turn 1 raw mismatch:\ngot:  %q\nwant: %q", events[0].Raw, sessLines[0])
	}
	if events[1].Raw != sessLines[1] {
		t.Errorf("turn 2 raw mismatch:\ngot:  %q\nwant: %q", events[1].Raw, sessLines[1])
	}
	if events[2].Raw != journalLines[0] {
		t.Errorf("tool announce raw mismatch:\ngot:  %q\nwant: %q", events[2].Raw, journalLines[0])
	}
	if events[3].Raw != journalLines[1] {
		t.Errorf("tool update raw mismatch:\ngot:  %q\nwant: %q", events[3].Raw, journalLines[1])
	}
}

// TestLateJoinerReplay verifies that a late joiner specifying last_n receives the
// exact last N events in order.
func TestLateJoinerReplay(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	agentID := "late-joiner-agent"
	agentDir := filepath.Join(tempDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	for i := 1; i <= 20; i++ {
		_ = AppendSessionTurn(agentDir, "user", fmt.Sprintf("Turn %d", i))
	}

	sdk := NewSDK(tempDir)
	resp, err := sdk.ReadSessionEvents(context.Background(), &agentv1.ReadSessionEventsRequest{
		AgentId:      agentID,
		WorkspaceDir: tempDir,
		Cursor:       &agentv1.ReadSessionEventsRequest_LastN{LastN: 5},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents last_n: %v", err)
	}

	if len(resp.Events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(resp.Events))
	}
	for i, ev := range resp.Events {
		expectedSeq := int64(16 + i)
		if ev.Seq != expectedSeq {
			t.Errorf("event %d: expected seq %d, got %d", i, expectedSeq, ev.Seq)
		}
	}
}

// mockSubscribeStream captures responses sent by SubscribeSession.
type mockSubscribeStream struct {
	grpc.ServerStreamingServer[agentv1.SubscribeSessionResponse]
	ctx       context.Context
	mu        sync.Mutex
	responses []*agentv1.SubscribeSessionResponse
}

func (m *mockSubscribeStream) Context() context.Context {
	return m.ctx
}

func (m *mockSubscribeStream) Send(resp *agentv1.SubscribeSessionResponse) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses = append(m.responses, resp)
	return nil
}

func (m *mockSubscribeStream) getResponses() []*agentv1.SubscribeSessionResponse {
	m.mu.Lock()
	defer m.mu.Unlock()
	copied := make([]*agentv1.SubscribeSessionResponse, len(m.responses))
	copy(copied, m.responses)
	return copied
}

// TestReplayThenLiveContinuity verifies that SubscribeSession seamlessly streams
// replayed past events, transitions to live streaming as new events are appended,
// and produces NO GAPS and NO DUPLICATES.
func TestReplayThenLiveContinuity(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	agentID := "replay-live-agent"
	agentDir := filepath.Join(tempDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// 1. Initial history: turns 1, 2, 3
	_ = AppendSessionTurn(agentDir, "user", "History 1")
	_ = AppendSessionTurn(agentDir, "model", "History 2")
	_ = AppendSessionTurn(agentDir, "user", "History 3")

	sdk := NewSDK(tempDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stream := &mockSubscribeStream{ctx: ctx}

	streamDone := make(chan error, 1)
	go func() {
		streamDone <- sdk.SubscribeSession(&agentv1.SubscribeSessionRequest{
			AgentId:      agentID,
			WorkspaceDir: tempDir,
			Cursor:       &agentv1.SubscribeSessionRequest_SinceSeq{SinceSeq: 1}, // replay from 2 onwards
		}, stream)
	}()

	// Wait for replay to deliver events 2 and 3
	var resps []*agentv1.SubscribeSessionResponse
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resps = stream.getResponses()
		if len(resps) >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(resps) < 2 {
		t.Fatalf("timed out waiting for replay events: got %d", len(resps))
	}

	// 2. Append live events: turn 4, tool 5, turn 6
	_ = AppendSessionTurn(agentDir, "model", "Live Turn 4")
	sink := NewToolEventSinkWithJournal(toolJournalPath(agentDir))
	sink.SetSeqAlloc(func() int64 {
		seq, _ := NextSeq(agentDir)
		return seq
	})
	callID := sink.Announce("test_tool", "arg=1", false)
	sink.Update(callID, "test_tool", "completed", 10, "ok", "")
	_ = AppendSessionTurn(agentDir, "user", "Live Turn 7")

	// Wait for live events to be delivered
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		resps = stream.getResponses()
		if len(resps) >= 6 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	<-streamDone

	resps = stream.getResponses()
	if len(resps) < 6 {
		t.Fatalf("expected at least 6 responses, got %d", len(resps))
	}

	// Verify exact sequence continuity: 2, 3, 4, 5, 6, 7
	expectedSeqs := []int64{2, 3, 4, 5, 6, 7}
	for i, exp := range expectedSeqs {
		if i >= len(resps) {
			t.Fatalf("missing response %d (expected seq %d)", i, exp)
		}
		ev := resps[i].GetEvent()
		if ev == nil {
			t.Fatalf("response %d has nil event", i)
		}
		if ev.Seq != exp {
			t.Errorf("response %d: expected seq %d, got %d", i, exp, ev.Seq)
		}
	}
}

// TestColdStartHeadOnlyAndLiveOnly verifies that cold-starting consumers can learn
// head seq metadata without receiving a batch, and live-only subscriptions emit nothing prior to join.
func TestColdStartHeadOnlyAndLiveOnly(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	agentID := "cold-agent"
	agentDir := filepath.Join(tempDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sdk := NewSDK(tempDir)

	_ = AppendSessionTurn(agentDir, "user", "turn 1")
	_ = AppendSessionTurn(agentDir, "model", "turn 2")

	sink := NewToolEventSinkWithJournal(toolJournalPath(agentDir))
	sink.SetSeqAlloc(func() int64 {
		seq, _ := NextSeq(agentDir)
		return seq
	})
	callID := sink.Announce("test_tool", "arg=cold", false)
	sink.Update(callID, "test_tool", "completed", 5, "res", "")

	// 1. Poll with HeadOnly: true (empty-batch cold start)
	resp, err := sdk.ReadSessionEvents(context.Background(), &agentv1.ReadSessionEventsRequest{
		AgentId:      agentID,
		WorkspaceDir: tempDir,
		Cursor:       &agentv1.ReadSessionEventsRequest_HeadOnly{HeadOnly: true},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents head_only: %v", err)
	}

	if len(resp.Events) != 0 {
		t.Fatalf("expected 0 events with head_only, got %d", len(resp.Events))
	}
	if resp.LatestSeq != 4 {
		t.Errorf("expected LatestSeq == 4, got %d", resp.LatestSeq)
	}
	if resp.LatestTurnSeq != 2 {
		t.Errorf("expected LatestTurnSeq == 2, got %d", resp.LatestTurnSeq)
	}
	if resp.BaselineSeq != 1 {
		t.Errorf("expected BaselineSeq == 1, got %d", resp.BaselineSeq)
	}

	// 2. Poll with SinceSeq == resp.LatestSeq: should return 0 events (posts nothing)
	pollResp, err := sdk.ReadSessionEvents(context.Background(), &agentv1.ReadSessionEventsRequest{
		AgentId:      agentID,
		WorkspaceDir: tempDir,
		Cursor:       &agentv1.ReadSessionEventsRequest_SinceSeq{SinceSeq: resp.LatestSeq},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents since_seq: %v", err)
	}
	if len(pollResp.Events) != 0 {
		t.Errorf("expected 0 events when starting at head seq, got %d", len(pollResp.Events))
	}

	// 3. New event arrives: should be delivered on next poll
	_ = AppendSessionTurn(agentDir, "user", "new turn 5")
	pollResp2, err := sdk.ReadSessionEvents(context.Background(), &agentv1.ReadSessionEventsRequest{
		AgentId:      agentID,
		WorkspaceDir: tempDir,
		Cursor:       &agentv1.ReadSessionEventsRequest_SinceSeq{SinceSeq: resp.LatestSeq},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents since_seq: %v", err)
	}
	if len(pollResp2.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(pollResp2.Events))
	}
	if pollResp2.Events[0].Seq != 5 {
		t.Errorf("expected seq 5, got %d", pollResp2.Events[0].Seq)
	}

	// 4. Subscribe with live_only: true emits nothing prior to subscription
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &mockSubscribeStream{ctx: ctx}
	streamDone := make(chan error, 1)

	go func() {
		streamDone <- sdk.SubscribeSession(&agentv1.SubscribeSessionRequest{
			AgentId:      agentID,
			WorkspaceDir: tempDir,
			Cursor:       &agentv1.SubscribeSessionRequest_LiveOnly{LiveOnly: true},
		}, stream)
	}()

	time.Sleep(100 * time.Millisecond)
	if len(stream.getResponses()) != 0 {
		t.Fatalf("expected 0 responses from live-only subscription before new events, got %d", len(stream.getResponses()))
	}

	// Emit live event 6
	_ = AppendSessionTurn(agentDir, "model", "live turn 6")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(stream.getResponses()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-streamDone

	resps := stream.getResponses()
	if len(resps) != 1 {
		t.Fatalf("expected 1 live response, got %d", len(resps))
	}
	if resps[0].GetEvent().GetSeq() != 6 {
		t.Errorf("expected seq 6, got %d", resps[0].GetEvent().GetSeq())
	}
}

// TestRollbackRestoredDirRewindSignalling verifies that when an agent's directory is
// rolled back or restored to a point where client cursor > latest_seq, the server
// signals rewound=true and rolled_back=true instead of stalling silently.
func TestRollbackRestoredDirRewindSignalling(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	agentID := "rollback-agent"
	agentDir := filepath.Join(tempDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sdk := NewSDK(tempDir)

	for i := 1; i <= 5; i++ {
		_ = AppendSessionTurn(agentDir, "user", fmt.Sprintf("Turn %d", i))
	}

	// Client has cursor at 100, but session only has events up to 5 (e.g. rolled back directory)
	resp, err := sdk.ReadSessionEvents(context.Background(), &agentv1.ReadSessionEventsRequest{
		AgentId:      agentID,
		WorkspaceDir: tempDir,
		Cursor:       &agentv1.ReadSessionEventsRequest_SinceSeq{SinceSeq: 100},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents: %v", err)
	}

	if !resp.Rewound {
		t.Errorf("expected rewound=true when since_seq (100) > latest_seq (5)")
	}
	if !resp.RolledBack {
		t.Errorf("expected rolled_back=true when since_seq (100) > latest_seq (5)")
	}
	if resp.LatestSeq != 5 {
		t.Errorf("expected latest_seq=5, got %d", resp.LatestSeq)
	}
	if len(resp.Events) != 5 {
		t.Errorf("expected all 5 events delivered on rollback reset, got %d", len(resp.Events))
	}

	// Also test streaming path
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &mockSubscribeStream{ctx: ctx}
	streamDone := make(chan error, 1)

	go func() {
		streamDone <- sdk.SubscribeSession(&agentv1.SubscribeSessionRequest{
			AgentId:      agentID,
			WorkspaceDir: tempDir,
			Cursor:       &agentv1.SubscribeSessionRequest_SinceSeq{SinceSeq: 100},
		}, stream)
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	<-streamDone

	resps := stream.getResponses()
	if len(resps) == 0 {
		t.Fatalf("expected responses from SubscribeSession on rollback")
	}
	if !resps[0].Rewound || !resps[0].RolledBack {
		t.Errorf("expected first frame to have Rewound=true and RolledBack=true, got: %+v", resps[0])
	}
}

// TestReplayThenLivePromptWakeOrdering verifies that registering the broker listener
// before reading the initial snapshot read enables prompt in-process wake notification
// without waiting for the 50ms polling fallback ticker.
func TestReplayThenLivePromptWakeOrdering(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	agentID := "prompt-wake-agent"
	agentDir := filepath.Join(tempDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sdk := NewSDK(tempDir)

	_ = AppendSessionTurn(agentDir, "user", "turn 1")
	_ = AppendSessionTurn(agentDir, "model", "turn 2")

	// Set fallback ticker to 5 seconds so prompt delivery relies strictly on the broker wake path.
	origTicker := subscribeTickerInterval
	subscribeTickerInterval = 5 * time.Second
	defer func() { subscribeTickerInterval = origTicker }()

	// Hook into the race window between register() and ReadSessionEventsFromDisk
	testHookPreSnapshotRead = func() {
		_ = AppendSessionTurn(agentDir, "user", "turn 3 during snapshot race")
	}
	defer func() { testHookPreSnapshotRead = nil }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &mockSubscribeStream{ctx: ctx}
	streamDone := make(chan error, 1)

	go func() {
		streamDone <- sdk.SubscribeSession(&agentv1.SubscribeSessionRequest{
			AgentId:      agentID,
			WorkspaceDir: tempDir,
		}, stream)
	}()

	// Wait for all 3 turns to be delivered via prompt wake (far before the 5-second ticker)
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if len(stream.getResponses()) >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-streamDone

	resps := stream.getResponses()
	if len(resps) != 3 {
		t.Fatalf("expected exactly 3 responses via prompt wake, got %d", len(resps))
	}
	for i, exp := range []int64{1, 2, 3} {
		if resps[i].GetEvent().GetSeq() != exp {
			t.Errorf("response %d: expected seq %d, got %d", i, exp, resps[i].GetEvent().GetSeq())
		}
	}
}

// TestScannerErrorPropagation verifies that oversized lines (> 16MB) return an explicit
// scanner error rather than silently returning a truncated slice or corrupting sequence recovery.
func TestScannerErrorPropagation(t *testing.T) {
	tempDir := t.TempDir()
	agentDir := filepath.Join(tempDir, "oversized-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write an oversized line (17MB, exceeding 16MB bufio.Scanner buffer cap)
	sessionPath := filepath.Join(agentDir, SessionFileName)
	oversized := make([]byte, 17*1024*1024)
	for i := range oversized {
		oversized[i] = 'a'
	}
	oversized[len(oversized)-1] = '\n'
	if err := os.WriteFile(sessionPath, oversized, 0644); err != nil {
		t.Fatalf("write oversized session: %v", err)
	}

	// ReadSessionEventsFromDisk must fail loudly, NOT return a truncated slice with err == nil
	_, _, _, _, err := ReadSessionEventsFromDisk(agentDir)
	if err == nil {
		t.Fatalf("expected ReadSessionEventsFromDisk to return scanner error on 17MB line, got nil")
	}
	if !strings.Contains(err.Error(), "token too long") {
		t.Errorf("expected error to mention 'token too long', got: %v", err)
	}

	// RecoverSeq must also fail loudly rather than recovering a low sequence number
	_, err = RecoverSeq(agentDir)
	if err == nil {
		t.Fatalf("expected RecoverSeq to return scanner error on 17MB line, got nil")
	}
	if !strings.Contains(err.Error(), "token too long") {
		t.Errorf("expected error to mention 'token too long', got: %v", err)
	}
}

// TestSubscribeSessionBurstDeliveryWithoutLoss verifies that when a burst of events
// exceeds maxSubscriberBuffer (64), head trimming preserves the oldest events and all
// durable events are delivered across chunked passes without data loss.
func TestSubscribeSessionBurstDeliveryWithoutLoss(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	agentID := "burst-agent"
	agentDir := filepath.Join(tempDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sdk := NewSDK(tempDir)

	_ = AppendSessionTurn(agentDir, "user", "turn 1")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &mockSubscribeStream{ctx: ctx}
	streamDone := make(chan error, 1)

	go func() {
		streamDone <- sdk.SubscribeSession(&agentv1.SubscribeSessionRequest{
			AgentId:      agentID,
			WorkspaceDir: tempDir,
		}, stream)
	}()

	// Wait for turn 1 replay
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(stream.getResponses()) >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Now burst 100 turns in one batch (exceeding maxSubscriberBuffer = 64)
	var lines []string
	for i := 2; i <= 101; i++ {
		pt := PersistedTurn{
			Role:  "user",
			Parts: []*genai.Part{{Text: fmt.Sprintf("burst turn %d", i)}},
			Seq:   int64(i),
		}
		data, _ := json.Marshal(pt)
		lines = append(lines, string(data))
	}
	f, err := os.OpenFile(filepath.Join(agentDir, SessionFileName), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("open session.jsonl: %v", err)
	}
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatalf("write session lines: %v", err)
	}
	_ = f.Close()
	_ = SetSeq(agentDir, 101)
	NotifySessionActivity(agentDir)

	// Wait for stream to deliver all 101 buffered events without loss
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(stream.getResponses()) >= 101 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-streamDone

	resps := stream.getResponses()
	if len(resps) != 101 {
		t.Fatalf("expected all 101 responses delivered without loss, got %d", len(resps))
	}

	for i, r := range resps {
		expectedSeq := int64(i + 1)
		if r.GetEvent().GetSeq() != expectedSeq {
			t.Errorf("event %d: expected seq %d, got %d", i, expectedSeq, r.GetEvent().GetSeq())
		}
	}
}

// TestSeqCrossProcessWorker is invoked as a subprocess by TestSeqMonotonicityCrossProcessWriters.
func TestSeqCrossProcessWorker(t *testing.T) {
	agentDir := os.Getenv("TEST_SEQ_WORKER_AGENT_DIR")
	if agentDir == "" {
		return
	}
	attemptsStr := os.Getenv("TEST_SEQ_WORKER_ATTEMPTS")
	attempts, _ := strconv.Atoi(attemptsStr)
	if attempts <= 0 {
		attempts = 25
	}
	for i := 0; i < attempts; i++ {
		seq, err := NextSeq(agentDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "worker error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("ALLOCATED_SEQ:%d\n", seq)
	}
	os.Exit(0)
}

// TestSeqMonotonicityCrossProcessWriters verifies that sequence numbers allocated across
// multiple independent OS processes are strictly monotonic, unique, and free of temp-file collisions.
func TestSeqMonotonicityCrossProcessWriters(t *testing.T) {
	tempDir := t.TempDir()
	agentDir := filepath.Join(tempDir, "cross-proc-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	numProcesses := 5
	attemptsPerProcess := 25
	totalAllocations := numProcesses * attemptsPerProcess

	type workerResult struct {
		output string
		err    error
	}
	results := make(chan workerResult, numProcesses)

	for p := 0; p < numProcesses; p++ {
		go func(workerID int) {
			cmd := exec.Command(os.Args[0], "-test.run=TestSeqCrossProcessWorker", "--")
			cmd.Env = append(os.Environ(),
				"TEST_SEQ_WORKER_AGENT_DIR="+agentDir,
				fmt.Sprintf("TEST_SEQ_WORKER_ATTEMPTS=%d", attemptsPerProcess),
			)
			out, err := cmd.CombinedOutput()
			results <- workerResult{output: string(out), err: err}
		}(p)
	}

	var allSeqs []int64
	for p := 0; p < numProcesses; p++ {
		res := <-results
		if res.err != nil {
			t.Fatalf("worker failed: %v, output:\n%s", res.err, res.output)
		}
		lines := strings.Split(res.output, "\n")
		for _, line := range lines {
			if strings.HasPrefix(line, "ALLOCATED_SEQ:") {
				valStr := strings.TrimPrefix(line, "ALLOCATED_SEQ:")
				seq, err := strconv.ParseInt(strings.TrimSpace(valStr), 10, 64)
				if err != nil {
					t.Fatalf("invalid allocated seq %q: %v", valStr, err)
				}
				allSeqs = append(allSeqs, seq)
			}
		}
	}

	if len(allSeqs) != totalAllocations {
		t.Fatalf("expected %d total allocations, got %d", totalAllocations, len(allSeqs))
	}

	sort.Slice(allSeqs, func(i, j int) bool { return allSeqs[i] < allSeqs[j] })

	seen := make(map[int64]bool)
	for i, s := range allSeqs {
		if seen[s] {
			t.Fatalf("duplicate sequence number %d found across processes", s)
		}
		seen[s] = true
		expected := int64(i + 1)
		if s != expected {
			t.Errorf("gap or jump at index %d: expected seq %d, got %d", i, expected, s)
		}
	}

	cur, err := CurrentSeq(agentDir)
	if err != nil {
		t.Fatalf("CurrentSeq: %v", err)
	}
	if cur != int64(totalAllocations) {
		t.Errorf("expected CurrentSeq=%d, got %d", totalAllocations, cur)
	}
}

// TestCorruptedSeqFileRecovery verifies that a corrupted session.seq file is automatically
// recovered using RecoverSeq rather than permanently bricking sequence number allocation.
func TestCorruptedSeqFileRecovery(t *testing.T) {
	tempDir := t.TempDir()
	agentDir := filepath.Join(tempDir, "corrupted-seq-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Add 3 turns to session.jsonl with seq 1, 2, 3
	for i := 1; i <= 3; i++ {
		_ = AppendSessionTurn(agentDir, "user", fmt.Sprintf("Turn %d", i))
	}

	// Corrupt session.seq with garbage text
	seqPath := filepath.Join(agentDir, SeqFileName)
	if err := os.WriteFile(seqPath, []byte("garbage_not_a_number\n"), 0644); err != nil {
		t.Fatalf("write corrupt seq: %v", err)
	}

	// NextSeq must not fail permanently; it must recover from session.jsonl and yield 4
	seq, err := NextSeq(agentDir)
	if err != nil {
		t.Fatalf("NextSeq failed on corrupt session.seq: %v", err)
	}
	if seq != 4 {
		t.Errorf("expected recovered seq=4, got %d", seq)
	}

	// Verify session.seq on disk is now healed
	cur, err := CurrentSeq(agentDir)
	if err != nil {
		t.Fatalf("CurrentSeq failed after heal: %v", err)
	}
	if cur != 4 {
		t.Errorf("expected CurrentSeq=4, got %d", cur)
	}
}

// TestZeroSeqJournalRowRewindSignalling verifies that a journal row with seq <= 0 does not
// yield baseline=0 and disable the since == 0 && baseline > 1 rewind rule.
func TestZeroSeqJournalRowRewindSignalling(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	agentDir := filepath.Join(tempDir, "zeroseq-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	sdk := NewSDK(tempDir)

	// Turn at seq 5
	_ = AppendSessionContentWithSeq(agentDir, genai.NewContentFromText("turn 5", "user"), 5)

	// Journal row with seq 0 (legacy or unsequenced)
	jPath := toolJournalPath(agentDir)
	jData, _ := json.Marshal(ToolEvent{CallID: "c0", ToolName: "tool0", Seq: 0, Timestamp: time.Now()})
	_ = os.WriteFile(jPath, []byte(string(jData)+"\n"), 0644)

	// Poll with since_seq == 0
	resp, err := sdk.ReadSessionEvents(context.Background(), &agentv1.ReadSessionEventsRequest{
		AgentId:      "zeroseq-agent",
		WorkspaceDir: tempDir,
		Cursor:       &agentv1.ReadSessionEventsRequest_SinceSeq{SinceSeq: 0},
	})
	if err != nil {
		t.Fatalf("ReadSessionEvents: %v", err)
	}

	if resp.BaselineSeq <= 0 {
		t.Fatalf("expected baselineSeq > 0, got %d", resp.BaselineSeq)
	}
	if !resp.Rewound {
		t.Errorf("expected rewound=true when baselineSeq (%d) > 1 and since_seq=0", resp.BaselineSeq)
	}
}
