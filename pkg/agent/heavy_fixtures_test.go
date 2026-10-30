//go:build heavy_payload

// These are the scale fixtures. Two classes live here: tests that need a turn larger than the
// 64MiB persist cap, and tests that need a log line larger than the 128MiB scanner bound. The
// behaviour under test only exists past those caps, so the fixtures cannot simply be made smaller;
// what can be chosen is whether the 4GB dev host pays for them on every gates run.
//
// Measured peak RSS of the race-instrumented pkg/agent test binary while each runs alone (4GB Pi,
// 2026-10-30):
//
//   TestAppendDeferredImages_DropIsLoud             2755 MB  9216x9216 noise image, whose JPEG
//                                                  re-encode must exceed the persist cap
//   TestPersistTurn_OverCapDropIsReported           1126 MB  64MiB image, marshaled to ~85MiB
//   TestAppendSessionContent_WholeContentFallback   1012 MB  49MiB inline image
//   TestPersistTurn_40MiBImageRoundTrip              984 MB  40MiB image, a ~53MiB log line
//   TestRecoverMaxSeqFromLog_ScannerErrorIsUnrecoverable 1253 MB  line past the scanner bound
//   TestStreamHandler_RePanicsOnUnrecoverableError  1252 MB  line past the scanner bound
//   TestTailMaxSeq_OverBoundFinalLine               1217 MB  line past the scanner bound
//   TestSubscribeSession_CorruptSessionDoesNotHotPoll  828 MB  oversized line in a live session
//   TestScannerErrorPropagation                      825 MB  oversized line in a live session
//   TestD101_ErrorTransparency_ReadSessionError         1095 MB  tool writes the oversized line, the turn reads it back
//
// Every other test in the package peaks at roughly 100 MB, so these ten are what drove a full
// go test -race ./... to the 2.4-2.5GB the kernel OOM killer acted on twice (bug card
// bugs/wackypub/race-suite-oom-pi). They are behind a build tag so the default gates run stays
// clear of that line, and CI runs them explicitly on a runner with the headroom:
//
//   go test ./pkg/agent/ -tags heavy_payload -race -count=1 -run '<the ten names, listed in ci.yml>'
//
// That run list lives in .github/workflows/ci.yml. Do not trim a test from it.
// TestHeavyFixturesStayBehindTheHeavyPayloadTag fails the default run if a heavy test is missing
// from CI, or if a fixture of this scale appears in an untagged file.
// reappears in an untagged file.

package agent

import (
	"bytes"
	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"image"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/genai"

	"context"

	"encoding/json"
	"fmt"

	"google.golang.org/grpc"

	"sync/atomic"

	"time"

	"io"

	"net/http"

	"net/http/httptest"
)

// TestPersistTurn_40MiBImageRoundTrip is the acceptance case for the cap raise: a
// 40MiB queued image survives persist -> read byte-identical, and seq recovery
// still works with a 53MiB final line in the log.
func TestPersistTurn_40MiBImageRoundTrip(t *testing.T) {
	agentDir := t.TempDir()

	imageData := make([]byte, 40*1024*1024)
	for i := range imageData {
		imageData[i] = byte(i % 251)
	}
	content := &genai.Content{
		Role: "user",
		Parts: []*genai.Part{
			{Text: "<IMAGE>The following image is stored in scratchpad 'rt40'</IMAGE>"},
			{InlineData: &genai.Blob{MIMEType: "image/jpeg", Data: imageData}},
		},
	}

	seq, report, err := AppendSessionContentGetSeq(agentDir, content)
	if err != nil {
		t.Fatalf("AppendSessionContentGetSeq: %v", err)
	}
	if report.Dropped() || report.TruncatedBytes != 0 {
		t.Fatalf("40MiB image must not be dropped: %+v", report)
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}
	parts := turns[0].Parts
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d", len(parts))
	}
	if !bytes.Equal(parts[1].InlineData.Data, imageData) {
		t.Fatalf("image round-trip not byte-identical: got %d bytes, want %d", len(parts[1].InlineData.Data), len(imageData))
	}

	// The persisted line is ~53MiB (base64 of 40MiB) and must fit the cap.
	raw, err := os.ReadFile(filepath.Join(agentDir, SessionFileName))
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	if len(raw) > MaxPersistTurnBytes+1 {
		t.Errorf("persisted line %d exceeds cap %d", len(raw), MaxPersistTurnBytes)
	}

	// Seq recovery with a 53MiB final line: allocation must continue, not restart.
	if got, err := CurrentSeq(agentDir); err != nil || got != seq {
		t.Errorf("CurrentSeq after 53MiB final line: got %d (%v), want %d", got, err, seq)
	}
	if got, err := NextSeq(agentDir); err != nil || got != seq+1 {
		t.Errorf("NextSeq after 53MiB final line: got %d (%v), want %d", got, err, seq+1)
	}
}

// TestPersistTurn_OverCapDropIsReported pins the loud-drop contract at the marshal
// boundary: a turn whose marshaled form exceeds MaxPersistTurnBytes loses its
// non-text parts, keeps the first text part plus the banner, and reports the drop
// with the exact byte count.
func TestPersistTurn_OverCapDropIsReported(t *testing.T) {
	agentDir := t.TempDir()

	// MaxPersistTurnBytes of raw image data marshals to ~1.33x the cap - guaranteed overflow.
	imageData := make([]byte, MaxPersistTurnBytes)
	content := &genai.Content{
		Role: "user",
		Parts: []*genai.Part{
			{Text: "keep me"},
			{InlineData: &genai.Blob{MIMEType: "image/jpeg", Data: imageData}},
		},
	}

	report, err := AppendSessionContent(agentDir, content)
	if err != nil {
		t.Fatalf("AppendSessionContent: %v", err)
	}
	if report.DroppedParts != 1 {
		t.Errorf("expected DroppedParts 1, got %d", report.DroppedParts)
	}
	if report.DroppedBytes != int64(len(imageData)) {
		t.Errorf("expected DroppedBytes %d, got %d", len(imageData), report.DroppedBytes)
	}

	raw, err := os.ReadFile(filepath.Join(agentDir, SessionFileName))
	if err != nil {
		t.Fatalf("read session file: %v", err)
	}
	if len(raw) > MaxPersistTurnBytes+1 {
		t.Errorf("persisted line %d exceeds cap %d - the hard invariant is broken", len(raw), MaxPersistTurnBytes)
	}
	if !strings.Contains(string(raw), "remaining content dropped") {
		t.Errorf("expected drop banner in persisted line")
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns: %v", err)
	}
	if len(turns) != 1 || len(turns[0].Parts) != 2 {
		t.Fatalf("expected 1 turn with 2 parts, got %+v", turns)
	}
	if turns[0].Parts[0].Text != "keep me" {
		t.Errorf("first text part not preserved: %q", turns[0].Parts[0].Text)
	}
	if !strings.Contains(turns[0].Parts[1].Text, "exceeded") {
		t.Errorf("second part should be the drop banner, got %q", turns[0].Parts[1].Text)
	}
}

// TestAppendDeferredImages_DropIsLoud is the end-to-end form of the silent-drop
// fix: a queued scratchpad image that overflows the persist cap must surface as an
// explicit <IMAGE_ERROR> turn the model can see (plus the banner in the image
// turn), not as a silent absence. A small image of the same shape must persist
// intact with no error turn.
func TestAppendDeferredImages_DropIsLoud(t *testing.T) {
	agentDir := t.TempDir()
	spDir := filepath.Join(agentDir, ScratchpadDirName)
	if err := os.MkdirAll(spDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Large: 9216x9216 noise re-encodes past the persist cap (fixture verifies its
	// own premise so an encoder change fails the test instead of the fixture).
	large := encodeNoiseJPEG(t, 9216)
	if int64(len(large))*4/3 < MaxPersistTurnBytes {
		t.Fatalf("fixture: re-encoded noise %d bytes (base64 %d) does not exceed cap %d", len(large), len(large)*4/3, MaxPersistTurnBytes)
	}
	if err := os.WriteFile(filepath.Join(spDir, "bigspima-0-agent.dat"), large, 0644); err != nil {
		t.Fatal(err)
	}
	small := encodeNoiseJPEG(t, 32)
	if err := os.WriteFile(filepath.Join(spDir, "smllspim-0-agent.dat"), small, 0644); err != nil {
		t.Fatal(err)
	}

	fa := &FolderAgent{
		AgentDir:      agentDir,
		AgentID:       "imgagent",
		RuntimeConfig: &RuntimeConfig{MaxImageDimension: 100000},
	}

	// Plain tmp dir is not a git workspace, so CommitWorkspaceEvent no-ops.
	valid := fa.appendDeferredImages("", []string{"bigspima", "smllspim"})
	if valid != 2 {
		t.Fatalf("expected 2 valid image turns, got %d", valid)
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns: %v", err)
	}
	// Expected shape: [big image turn (banner), big IMAGE_ERROR turn, small image turn]
	if len(turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(turns))
	}
	if !strings.Contains(turns[0].Parts[len(turns[0].Parts)-1].Text, "remaining content dropped") {
		t.Errorf("big image turn should carry the drop banner, got %q", turns[0].Parts[len(turns[0].Parts)-1].Text)
	}
	if len(turns[1].Parts) != 1 || !strings.Contains(turns[1].Parts[0].Text, "<IMAGE_ERROR>") || !strings.Contains(turns[1].Parts[0].Text, "bigspima") {
		t.Errorf("expected an explicit IMAGE_ERROR turn for bigspima, got %+v", turns[1].Parts)
	}
	smallPart := turns[2].Parts
	if len(smallPart) != 2 || smallPart[1].InlineData == nil {
		t.Errorf("small image turn should persist its inline image, got %+v", smallPart)
	} else {
		// The persist layer is byte-faithful to what the normalizer produced (the
		// round-trip fidelity itself is pinned by TestPersistTurn_40MiBImageRoundTrip);
		// the decode check confirms the persisted bytes are the intact 32x32 image.
		decoded, _, derr := image.Decode(bytes.NewReader(smallPart[1].InlineData.Data))
		if derr != nil || decoded.Bounds().Dx() != 32 || decoded.Bounds().Dy() != 32 {
			t.Errorf("persisted small image should decode to 32x32, got %v (%v)", decoded, derr)
		}
	}
	_ = small
	if strings.Contains(turns[2].Parts[0].Text, "IMAGE_ERROR") {
		t.Errorf("small image must not produce an error turn")
	}
}

// TestAppendSessionContent_WholeContentFallback came from session_store_test.go. It needs
// 49MiB of inline image data to push a marshaled turn past the 64MiB cap, so it is part of the
// payload-scale set described at the top of this file: measured peak RSS under -race is 1012MB.

func TestAppendSessionContent_WholeContentFallback(t *testing.T) {
	agentDir := t.TempDir()

	// The whole-turn fallback is exercised with an image part: text parts are capped
	// individually at MaxPersistTextPartBytes (256KB), so a text-only turn can no longer
	// exceed the 64MiB turn cap on its own - the fallback is the image path. 49MiB of
	// InlineData marshals to ~65MiB of base64, past the 64MiB cap, so the fallback must
	// keep only the first text part plus the drop banner.
	image := make([]byte, 49*1024*1024)
	part1 := "PART1_" + strings.Repeat("a", 200*1024-6)

	content := &genai.Content{
		Role: "user",
		Parts: []*genai.Part{
			{Text: part1},
			{InlineData: &genai.Blob{MIMEType: "image/jpeg", Data: image}},
		},
	}

	report, err := AppendSessionContent(agentDir, content)
	if err != nil {
		t.Fatalf("AppendSessionContent failed: %v", err)
	}

	rawFile, err := os.ReadFile(filepath.Join(agentDir, SessionFileName))
	if err != nil {
		t.Fatalf("os.ReadFile failed: %v", err)
	}

	// Marshaled turn on disk must not exceed MaxPersistTurnBytes + newline
	if len(rawFile) > MaxPersistTurnBytes+1 {
		t.Errorf("persisted turn size %d exceeds MaxPersistTurnBytes %d", len(rawFile), MaxPersistTurnBytes)
	}

	turns, err := ReadSessionTurns(agentDir)
	if err != nil {
		t.Fatalf("ReadSessionTurns failed: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}
	// Fallback should keep only the first text part plus the drop banner
	if len(turns[0].Parts) != 2 {
		t.Fatalf("expected 2 parts (first text part + banner), got %d", len(turns[0].Parts))
	}
	if !strings.HasPrefix(turns[0].Parts[0].Text, "PART1_") {
		t.Errorf("expected first part to be preserved, got: %q", turns[0].Parts[0].Text[:50])
	}
	if !strings.Contains(turns[0].Parts[1].Text, "remaining content dropped") {
		t.Errorf("expected second part to be drop banner, got: %q", turns[0].Parts[1].Text)
	}

	// The drop must be reported, not silent: one part (the image) of known size.
	if report.DroppedParts != 1 {
		t.Errorf("expected DroppedParts 1, got %d", report.DroppedParts)
	}
	if report.DroppedBytes != int64(len(image)) {
		t.Errorf("expected DroppedBytes %d, got %d", len(image), report.DroppedBytes)
	}
}

// The five tests below are the second scale class: they prove behaviour at the scanner bound, so
// each writes a line larger than MaxSessionLineBytes (128MiB). Measured peak RSS of the race
// binary while each runs alone: 1253MB, 1252MB, 1217MB, 828MB and 825MB.
// (moved from robustness_test.go)
// TestRecoverMaxSeqFromLog_ScannerErrorIsUnrecoverable verifies the #88 fail-closed path
// now carries the unrecoverable marker: a session log with a line over the MaxSessionLineBytes
// scanner cap cannot be safely allocated from, so the error is marked corrupt-state, which the
// #90 supervision (stdio-serve / ProcessDialer) turns into a process restart instead of
// failing every subsequent turn individually.
func TestRecoverMaxSeqFromLog_ScannerErrorIsUnrecoverable(t *testing.T) {
	agentDir := t.TempDir()
	// One small turn, then a line far beyond the MaxSessionLineBytes scanner buffer.
	turns := []*genai.Content{genai.NewContentFromText("seed", "user")}
	if err := WriteSessionTurns(agentDir, turns); err != nil {
		t.Fatalf("WriteSessionTurns: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(agentDir, SessionFileName), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	// testScannerOverflowLine - exceeds the scanner buffer (MaxSessionLineBytes).
	if _, err := f.Write(append([]byte(strings.Repeat("x", testScannerOverflowLine)), '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()

	_, err = recoverMaxSeqFromLog(agentDir)
	if err == nil {
		t.Fatal("expected scanner error from over-cap line")
	}
	if !IsUnrecoverable(err) {
		t.Fatalf("scanner error should be marked unrecoverable, got: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot be scanned for sequence inference") {
		t.Errorf("expected reason in error, got: %v", err)
	}
}

// (moved from robustness_test.go)
// TestStreamHandler_RePanicsOnUnrecoverableError verifies the error-branch of the
// StreamHandler recover: an RPC that RETURNS an unrecoverable error (not just panics)
// must re-panic so supervision restarts - a corrupt session becomes a server-level
// restart, not a per-turn protocol error. This drives a REAL corrupt session (17MiB
// line) through AddAndGenerateTurnStream so the append fails with the unrecoverable
// error, exercising the returned-error branch, not the panic-value branch.
func TestStreamHandler_RePanicsOnUnrecoverableError(t *testing.T) {
	wsDir := t.TempDir()
	t.Chdir(wsDir)
	agentID := "unrecoverable-agent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := WriteSessionTurns(agentDir, []*genai.Content{genai.NewContentFromText("seed", "user")}); err != nil {
		t.Fatalf("WriteSessionTurns: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(agentDir, SessionFileName), os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append([]byte(strings.Repeat("x", testScannerOverflowLine)), '\n')); err != nil {
		t.Fatal(err)
	}
	f.Close()

	sdk := NewSDK(wsDir)
	router := NewRoutingServer(sdk)

	t.Setenv("WACKYPUB_ALLOWED_AGENTS", "*")
	mockRequest = &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:      agentID,
		WorkspaceDir: wsDir,
		UserMessage:  "hello",
	}
	defer func() { mockRequest = nil }()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected unrecoverable to re-panic through the error branch, got nil")
		}
		if !IsUnrecoverable(r) {
			t.Fatalf("expected unrecoverable panic, got %v", r)
		}
	}()

	ctx := grpc.NewContextWithServerTransportStream(context.Background(), &mockServerTransportStream{method: "/wackypub.agent.v1.AgentService/AddAndGenerateTurnStream"})
	mock := &mockServerStream{ctx: ctx}
	_ = router.StreamHandler(nil, mock)
}

// (moved from seq_tail_test.go)
func TestTailMaxSeq_OverBoundFinalLine(t *testing.T) {
	t.Run("parseable over-bound line keeps its stamped seq", func(t *testing.T) {
		agentDir := t.TempDir()
		if s := appendTurns(t, agentDir, 2, "small"); s != 3 {
			t.Fatalf("fixture: expected final seq 3, got %d", s)
		}
		big := genai.NewContentFromText(strings.Repeat("z", testOverBoundLine), "user")
		line, err := json.Marshal(PersistedTurn{Content: *big, Seq: 3 + 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(line) <= tailMaxSeek {
			t.Fatalf("fixture line %dB does not exceed the seek cap %dB", len(line), tailMaxSeek)
		}
		f, err := os.OpenFile(filepath.Join(agentDir, SessionFileName), os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(append(line, '\n'))
		f.Close()

		got, err := tailMaxSeq(agentDir)
		if err != nil {
			t.Fatalf("tailMaxSeq: %v", err)
		}
		if got != 4 {
			t.Errorf("want stamped seq 4 from scan, got %d", got)
		}
	})

	t.Run("garbage over-bound line is skipped by the scan", func(t *testing.T) {
		agentDir := t.TempDir()
		if s := appendTurns(t, agentDir, 2, "small"); s != 3 {
			t.Fatalf("fixture: expected final seq 3, got %d", s)
		}
		f, err := os.OpenFile(filepath.Join(agentDir, SessionFileName), os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			t.Fatal(err)
		}
		f.Write(append([]byte(strings.Repeat("g", testOverBoundLine)), '\n'))
		f.Close()

		got, err := tailMaxSeq(agentDir)
		if err != nil {
			t.Fatalf("tailMaxSeq: %v", err)
		}
		if got != 3 {
			t.Errorf("want 3 (garbage line skipped), got %d", got)
		}
	})
}

// (moved from events_test.go)
// TestSubscribeSession_CorruptSessionDoesNotHotPoll verifies that an unreadable/corrupt
// session file (e.g. line exceeding MaxSessionLineBytes) does not cause the 50ms polling loop
// to spin re-opening and re-reading the unchanged corrupt file.
func TestSubscribeSession_CorruptSessionDoesNotHotPoll(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	agentID := "corrupt-agent"
	agentDir := filepath.Join(tempDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_ = AppendSessionTurn(agentDir, "user", "turn 1")

	var diskReadCount atomic.Int64
	origHook := testHookSessionDiskRead
	testHookSessionDiskRead = func() {
		diskReadCount.Add(1)
	}
	defer func() { testHookSessionDiskRead = origHook }()

	sdk := NewSDK(tempDir)
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
	time.Sleep(100 * time.Millisecond)
	readsAfterReplay := diskReadCount.Load()

	// Write the oversized line to a temp file and atomically replace session.jsonl
	// so the concurrent watcher does not observe an intermediate partially-written state.
	sessionPath := filepath.Join(agentDir, SessionFileName)
	oversized := make([]byte, testScannerOverflowLine)
	for i := range oversized {
		oversized[i] = 'a'
	}
	oversized[len(oversized)-1] = '\n'
	tmpPath := sessionPath + ".tmp"
	if err := os.WriteFile(tmpPath, oversized, 0644); err != nil {
		t.Fatalf("write tmp: %v", err)
	}
	if err := os.Rename(tmpPath, sessionPath); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// Wait for subscriber to attempt reading the replaced file
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && diskReadCount.Load() == readsAfterReplay {
		time.Sleep(20 * time.Millisecond)
	}
	if diskReadCount.Load() == readsAfterReplay {
		t.Fatalf("subscriber never attempted to read corrupted session")
	}
	// Give a short tick for the subscriber to set lastFileInfo and re-enter select
	time.Sleep(100 * time.Millisecond)
	readsAfterCorrupt := diskReadCount.Load()

	// Idle over 500ms (10 poll ticks): since the corrupt file did not change, zero further reads should occur
	time.Sleep(500 * time.Millisecond)
	readsAfterWait := diskReadCount.Load() - readsAfterCorrupt
	if readsAfterWait != 0 {
		t.Fatalf("expected 0 disk reads during corrupt idle period, got %d (still hot-polling)", readsAfterWait)
	}

	cancel()
	<-streamDone
}

// (moved from events_test.go)
// TestScannerErrorPropagation verifies that oversized lines (> 16MB) return an explicit
// scanner error rather than silently returning a truncated slice or corrupting sequence recovery.
func TestScannerErrorPropagation(t *testing.T) {
	tempDir := t.TempDir()
	agentDir := filepath.Join(tempDir, "oversized-agent")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write an oversized line (testScannerOverflowLine, exceeding MaxSessionLineBytes)
	sessionPath := filepath.Join(agentDir, SessionFileName)
	oversized := make([]byte, testScannerOverflowLine)
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
		t.Fatalf("expected ReadSessionEventsFromDisk to return scanner error on over-cap line, got nil")
	}
	if !strings.Contains(err.Error(), "token too long") {
		t.Errorf("expected error to mention 'token too long', got: %v", err)
	}

}

// (moved from continuation_test.go) Same scanner-bound class, and the test process itself reads
// the oversized line back after the tool writes it: 1095MB under -race.

func TestD101_ErrorTransparency_ReadSessionError(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "d101-read-err"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("failed creating agent dir: %v", err)
	}

	toolsDir := filepath.Join(agentDir, "tools")
	if err := os.MkdirAll(toolsDir, 0755); err != nil {
		t.Fatalf("failed creating tools dir: %v", err)
	}
	// Tool script writes a testScannerOverflowLine-length line to session.jsonl to trigger
	// scanner buffer overflow on post-turn read.
	toolScript := fmt.Sprintf("#!/bin/sh\npython3 -c 'print(\"x\" * %d)' >> %s/session.jsonl\necho 'done'\n", testScannerOverflowLine, agentDir)
	if err := os.WriteFile(filepath.Join(toolsDir, "corrupt_tool.sh"), []byte(toolScript), 0755); err != nil {
		t.Fatalf("failed creating tool script: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		toolCallJSON := `{
			"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_command","arguments":"{\"command\":\"corrupt_tool.sh\",\"args\":[]}"}}]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":90,"completion_tokens":10,"total_tokens":100}
		}`
		io.WriteString(w, toolCallJSON)
	}))
	defer srv.Close()

	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("System prompt"), 0644); err != nil {
		t.Fatalf("failed to write AGENTS.md: %v", err)
	}

	if err := AppendSessionTurn(agentDir, "user", "Start task"); err != nil {
		t.Fatalf("failed to write session.jsonl: %v", err)
	}

	runtimeCfg := &RuntimeConfig{
		Provider:      "openai",
		Model:         "test-model",
		Endpoint:      srv.URL,
		ContextWindow: 100,
	}
	runtimeData, _ := json.Marshal(runtimeCfg)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), runtimeData, 0644); err != nil {
		t.Fatalf("failed to write runtime.json: %v", err)
	}

	fa, err := LoadFolderAgent(wsDir, agentID, DefaultMaxToolTurns)
	if err != nil {
		t.Fatalf("LoadFolderAgent failed: %v", err)
	}

	// A line over the scanner cap cannot be safely inferred, so the post-turn append
	// fails CLOSED and surfaces the scanner error at allocation time, instead of the
	// old fail-open (allocate a duplicate seq; surface the corruption only later, at
	// the continuation read). The corruption is transparent either way, per D101.
	_, err = fa.GenerateTurn(context.Background())
	if err == nil {
		t.Fatalf("GenerateTurn should fail when session.jsonl holds an over-cap line")
	}
	if !strings.Contains(err.Error(), "token too long") {
		t.Errorf("expected token-too-long in error, got: %v", err)
	}
}
