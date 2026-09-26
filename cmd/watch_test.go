package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

func setupTestWatchAgent(t *testing.T) (wsDir, agentID, agentDir string) {
	t.Helper()
	watchRawFlag = false
	watchSinceSeqFlag = 0
	watchLastFlag = 0
	RootCmd.SetContext(nil)
	agentCmd.SetContext(nil)
	agentWatchCmd.SetContext(nil)
	t.Cleanup(func() {
		watchRawFlag = false
		watchSinceSeqFlag = 0
		watchLastFlag = 0
		RootCmd.SetContext(nil)
		agentCmd.SetContext(nil)
		agentWatchCmd.SetContext(nil)
	})
	wsDir = t.TempDir()
	t.Chdir(wsDir)

	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("write root marker: %v", err)
	}

	agentID = "watch-test-agent"
	agentDir = filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir agentDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("# test agent\n"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}

	return wsDir, agentID, agentDir
}

// TestCLIWatchDispatcher verifies that `wackypub agent <id> watch` routes correctly.
func TestCLIWatchDispatcher(t *testing.T) {
	wsDir, agentID, agentDir := setupTestWatchAgent(t)

	_ = adkAgent.AppendSessionTurn(agentDir, "user", "turn alpha")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", agentID, "watch", "--raw"})
	out, err := captureStdout(t, func() error {
		return RootCmd.ExecuteContext(ctx)
	})
	if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
		t.Fatalf("RootCmd.Execute failed: %v", err)
	}

	if !strings.Contains(out, "turn alpha") {
		t.Errorf("expected output to contain turn alpha, got:\n%s", out)
	}
}

// TestCLIWatchHumanReadable verifies the default human-readable renderer.
func TestCLIWatchHumanReadable(t *testing.T) {
	wsDir, agentID, agentDir := setupTestWatchAgent(t)

	_ = adkAgent.AppendSessionTurn(agentDir, "user", "hello human")
	sink := adkAgent.NewToolEventSink()
	sink.SetSeqAlloc(func() int64 {
		seq, _ := adkAgent.NextSeq(agentDir)
		return seq
	})
	callID := sink.Announce("create_file", "path=test.txt", false)
	sink.Update(callID, "create_file", "completed", 15, "created", "")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "watch", agentID})
	out, err := captureStdout(t, func() error {
		return RootCmd.ExecuteContext(ctx)
	})
	if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
		t.Fatalf("RootCmd.Execute failed: %v", err)
	}

	if !strings.Contains(out, "[Turn #1 user] hello human") {
		t.Errorf("expected human turn output, got:\n%s", out)
	}
	if !strings.Contains(out, "[Tool #2] create_file(path=test.txt)") {
		t.Errorf("expected human tool announce output, got:\n%s", out)
	}
	if !strings.Contains(out, "[Tool #3 COMPLETED] created") {
		t.Errorf("expected human tool update output, got:\n%s", out)
	}
}

// TestCLIWatchSinceSeq verifies the --since-seq flag filters past events.
func TestCLIWatchSinceSeq(t *testing.T) {
	wsDir, agentID, agentDir := setupTestWatchAgent(t)

	_ = adkAgent.AppendSessionTurn(agentDir, "user", "turn 1")
	_ = adkAgent.AppendSessionTurn(agentDir, "model", "turn 2")
	_ = adkAgent.AppendSessionTurn(agentDir, "user", "turn 3")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	RootCmd.SetArgs([]string{"--ws", wsDir, "agent", "watch", agentID, "--since-seq", "2", "--raw"})
	out, err := captureStdout(t, func() error {
		return RootCmd.ExecuteContext(ctx)
	})
	if err != nil && err != context.DeadlineExceeded && err != context.Canceled {
		t.Fatalf("RootCmd.Execute failed: %v", err)
	}

	if strings.Contains(out, "turn 1") || strings.Contains(out, "turn 2") {
		t.Errorf("expected turns 1 and 2 to be filtered by since-seq 2, got:\n%s", out)
	}
	if !strings.Contains(out, "turn 3") {
		t.Errorf("expected turn 3 to be present, got:\n%s", out)
	}
}
