package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/grpc"
)

type slowSubscribeStream struct {
	grpc.ServerStreamingServer[agentv1.SubscribeSessionResponse]
	ctx     context.Context
	mu      sync.Mutex
	turns   []*agentv1.SessionEvent
	sleepMs int
}

func (s *slowSubscribeStream) Context() context.Context { return s.ctx }

func (s *slowSubscribeStream) Send(resp *agentv1.SubscribeSessionResponse) error {
	s.mu.Lock()
	if resp.Event != nil {
		s.turns = append(s.turns, resp.Event)
	}
	s.mu.Unlock()
	time.Sleep(time.Duration(s.sleepMs) * time.Millisecond)
	return nil
}

func (s *slowSubscribeStream) seqs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int64, len(s.turns))
	for i, ev := range s.turns {
		out[i] = ev.Seq
	}
	return out
}

func (s *slowSubscribeStream) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.turns)
}

// TestRepro_MidReplayAppendDelivered appends a turn while the subscriber is still
// replaying its snapshot (between the snapshot read and the initial stat). The turn
// must still be delivered - the review focus "missed updates?".
func TestRepro_MidReplayAppendDelivered(t *testing.T) {
	tempDir := t.TempDir()
	t.Chdir(tempDir)
	agentID := "repro-window-agent"
	agentDir := filepath.Join(tempDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	_ = AppendSessionTurn(agentDir, "user", "History 1")
	_ = AppendSessionTurn(agentDir, "model", "History 2")

	sdk := NewSDK(tempDir)
	streamCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := &slowSubscribeStream{ctx: streamCtx, sleepMs: 300}

	done := make(chan error, 1)
	go func() {
		done <- sdk.SubscribeSession(&agentv1.SubscribeSessionRequest{AgentId: agentID, WorkspaceDir: tempDir}, stream)
	}()

	// Wait until both snapshot turns have been sent (replay in progress).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && stream.count() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if stream.count() < 2 {
		t.Fatalf("timed out waiting for snapshot replay")
	}

	// Append turn 3 while the subscriber is still in replay (slow Send sleeps 300ms
	// per event, so the stat after the replay loop has not run yet).
	if err := AppendSessionTurn(agentDir, "user", "Mid-replay turn 3"); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Give the watch loop time to deliver turn 3 (several ticks).
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := false
		for _, sq := range stream.seqs() {
			if sq == 3 {
				got = true
			}
		}
		if got {
			t.Log("mid-replay turn delivered")
			cancel()
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatalf("mid-replay turn 3 was never delivered (missed update): seqs seen = %v", stream.seqs())
}
