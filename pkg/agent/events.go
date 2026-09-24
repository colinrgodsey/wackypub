package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// SessionBroker manages in-process notification for live session events.
type SessionBroker struct {
	mu          sync.RWMutex
	subscribers map[string][]chan struct{}
}

var globalBroker = &SessionBroker{
	subscribers: make(map[string][]chan struct{}),
}

func (b *SessionBroker) register(agentDir string) (chan struct{}, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan struct{}, 8)
	b.subscribers[agentDir] = append(b.subscribers[agentDir], ch)
	cleanup := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		subs := b.subscribers[agentDir]
		for i, s := range subs {
			if s == ch {
				b.subscribers[agentDir] = append(subs[:i], subs[i+1:]...)
				break
			}
		}
		if len(b.subscribers[agentDir]) == 0 {
			delete(b.subscribers, agentDir)
		}
	}
	return ch, cleanup
}

func (b *SessionBroker) notify(agentDir string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subscribers[agentDir] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// NotifySessionActivity signals live subscribers for agentDir that new events were written.
func NotifySessionActivity(agentDir string) {
	globalBroker.notify(agentDir)
}

const maxSubscriberBuffer = 64

// testHookPreSnapshotRead is invoked immediately after broker registration and before reading the initial snapshot.
// Used in tests to verify that broker registration before snapshot read eliminates the race window.
var testHookPreSnapshotRead func()

// ReadSessionEventsFromDisk reads and merges all durable events from session.jsonl and tool-journal.jsonl.
func ReadSessionEventsFromDisk(agentDir string) ([]*agentv1.SessionEvent, int64, int64, int64, error) {
	var events []*agentv1.SessionEvent
	var latestTurnSeq int64
	var sessionBaselineSeq int64
	var hasCompaction bool

	// 1. Read session.jsonl
	sessionPath := filepath.Join(agentDir, SessionFileName)
	if f, err := os.Open(sessionPath); err == nil {
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		var lineIdx int64
		for scanner.Scan() {
			lineIdx++
			lineBytes := scanner.Bytes()
			if len(lineBytes) == 0 {
				continue
			}
			rawLine := string(lineBytes)

			var pt PersistedTurn
			if err := json.Unmarshal(lineBytes, &pt); err != nil {
				continue
			}

			seq := pt.Seq
			if seq <= 0 {
				seq = lineIdx
			}

			// Convert to proto turn
			st := &agentv1.SessionTurn{
				Role: pt.Role,
				Seq:  seq,
			}
			var isCompaction bool
			var compactionText string
			for _, p := range pt.Parts {
				if p == nil {
					continue
				}
				sp := &agentv1.SessionPart{
					Text: p.Text,
				}
				if p.InlineData != nil {
					sp.InlineData = p.InlineData.Data
					sp.MimeType = p.InlineData.MIMEType
				}
				st.Parts = append(st.Parts, sp)
				if strings.Contains(p.Text, "<COMPACTION_NOTICE>") {
					isCompaction = true
					hasCompaction = true
					compactionText = p.Text
				}
			}

			if !isCompaction {
				if sessionBaselineSeq == 0 || seq < sessionBaselineSeq {
					sessionBaselineSeq = seq
				}
				if seq > latestTurnSeq {
					latestTurnSeq = seq
				}
			} else if sessionBaselineSeq == 0 {
				sessionBaselineSeq = seq
			}

			sev := &agentv1.SessionEvent{
				Seq: seq,
				Raw: rawLine,
			}
			if isCompaction {
				sev.Event = &agentv1.SessionEvent_Compaction{
					Compaction: &agentv1.CompactionEvent{
						Summary: compactionText,
						Turn:    st,
					},
				}
			} else {
				sev.Event = &agentv1.SessionEvent_Turn{
					Turn: st,
				}
			}
			events = append(events, sev)
		}
		if err := scanner.Err(); err != nil {
			_ = f.Close()
			return nil, 0, 0, 0, fmt.Errorf("reading %s: %w", SessionFileName, err)
		}
		_ = f.Close()
	} else if !os.IsNotExist(err) {
		return nil, 0, 0, 0, fmt.Errorf("opening %s: %w", SessionFileName, err)
	}

	// 2. Read tool-journal.jsonl
	journalPath := toolJournalPath(agentDir)
	if f, err := os.Open(journalPath); err == nil {
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
		for scanner.Scan() {
			lineBytes := scanner.Bytes()
			if len(lineBytes) == 0 {
				continue
			}
			rawLine := string(lineBytes)

			var te ToolEvent
			if err := json.Unmarshal(lineBytes, &te); err != nil {
				continue
			}

			sev := &agentv1.SessionEvent{
				Seq:       te.Seq,
				Raw:       rawLine,
				Timestamp: timestamppb.New(te.Timestamp),
			}

			if te.ToolName == "compaction" {
				hasCompaction = true
				sev.Event = &agentv1.SessionEvent_Compaction{
					Compaction: &agentv1.CompactionEvent{
						Summary: te.ArgsSummary,
					},
				}
			} else if te.Status == "" && !te.Denied {
				sev.Event = &agentv1.SessionEvent_ToolCall{
					ToolCall: &agentv1.ToolCall{
						CallId:      te.CallID,
						ToolName:    te.ToolName,
						ArgsSummary: te.ArgsSummary,
						Denied:      te.Denied,
						Seq:         te.Seq,
					},
				}
			} else {
				sev.Event = &agentv1.SessionEvent_ToolCallUpdate{
					ToolCallUpdate: &agentv1.ToolCallUpdate{
						CallId:      te.CallID,
						ToolName:    te.ToolName,
						Status:      te.Status,
						ResultBytes: te.ResultBytes,
						ResultHead:  te.ResultHead,
						ResultRef:   te.ResultRef,
						Seq:         te.Seq,
					},
				}
			}
			events = append(events, sev)
		}
		if err := scanner.Err(); err != nil {
			_ = f.Close()
			return nil, 0, 0, 0, fmt.Errorf("reading tool journal: %w", err)
		}
		_ = f.Close()
	} else if !os.IsNotExist(err) {
		return nil, 0, 0, 0, fmt.Errorf("opening tool journal: %w", err)
	}

	// Sort strictly by sequence number
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Seq < events[j].Seq
	})

	var baselineSeq int64
	var latestSeq int64
	if hasCompaction && sessionBaselineSeq > 0 {
		// When compaction has occurred, session baseline from session stream defines the rewind boundary.
		// Older tool journal entries from compacted turns are dropped so they do not drag baseline backward.
		filtered := events[:0]
		for _, ev := range events {
			if ev.Seq >= sessionBaselineSeq {
				filtered = append(filtered, ev)
			}
		}
		events = filtered
		baselineSeq = sessionBaselineSeq
	} else if len(events) > 0 {
		baselineSeq = events[0].Seq
	}
	if len(events) > 0 {
		latestSeq = events[len(events)-1].Seq
	}
	cur, _ := CurrentSeq(agentDir)
	if cur > latestSeq {
		latestSeq = cur
	}

	return events, baselineSeq, latestSeq, latestTurnSeq, nil
}

// ReadSessionEvents implements agentv1.AgentServiceServer.ReadSessionEvents.
func (s *AgentSDK) ReadSessionEvents(ctx context.Context, req *agentv1.ReadSessionEventsRequest) (*agentv1.ReadSessionEventsResponse, error) {
	wsDir := s.WorkspaceDir
	if req != nil && req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentID := ""
	if req != nil {
		agentID = req.GetAgentId()
	}
	if agentID == "" {
		return nil, fmt.Errorf("agentID cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return nil, err
	}

	agentDir := filepath.Join(wsDir, agentID)
	events, baselineSeq, latestSeq, latestTurnSeq, err := ReadSessionEventsFromDisk(agentDir)
	if err != nil {
		return nil, err
	}

	// If head_only was requested, return metadata without batch of events (cold start optimization)
	if req != nil && req.GetHeadOnly() {
		return &agentv1.ReadSessionEventsResponse{
			Events:        nil,
			Rewound:       false,
			BaselineSeq:   baselineSeq,
			LatestSeq:     latestSeq,
			LatestTurnSeq: latestTurnSeq,
		}, nil
	}

	var filtered []*agentv1.SessionEvent
	rewound := false
	rolledBack := false

	if req != nil && req.GetSinceSeq() > 0 {
		since := req.GetSinceSeq()
		if since < baselineSeq {
			rewound = true
			filtered = events
		} else if latestSeq < since {
			// Rollback or restored directory: client cursor is ahead of current session head!
			rewound = true
			rolledBack = true
			filtered = events
		} else {
			for _, ev := range events {
				if ev.Seq > since {
					filtered = append(filtered, ev)
				}
			}
		}
	} else if req != nil && req.GetLastN() > 0 {
		n := int(req.GetLastN())
		if n < len(events) {
			filtered = events[len(events)-n:]
		} else {
			filtered = events
		}
	} else {
		// since_seq == 0 or unspecified: return all surviving events
		if baselineSeq > 1 {
			// Cursor was 0, but earlier turns were compacted
			rewound = true
		}
		filtered = events
	}

	return &agentv1.ReadSessionEventsResponse{
		Events:        filtered,
		Rewound:       rewound,
		BaselineSeq:   baselineSeq,
		LatestSeq:     latestSeq,
		LatestTurnSeq: latestTurnSeq,
		RolledBack:    rolledBack,
	}, nil
}

// SubscribeSession implements agentv1.AgentServiceServer.SubscribeSession.
func (s *AgentSDK) SubscribeSession(req *agentv1.SubscribeSessionRequest, stream grpc.ServerStreamingServer[agentv1.SubscribeSessionResponse]) error {
	wsDir := s.WorkspaceDir
	if req != nil && req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentID := ""
	if req != nil {
		agentID = req.GetAgentId()
	}
	if agentID == "" {
		return fmt.Errorf("agentID cannot be empty")
	}

	if err := AuthorizeAgentTarget(agentID); err != nil {
		return err
	}

	agentDir := filepath.Join(wsDir, agentID)
	ctx := stream.Context()

	// Register in-process notification listener before reading initial snapshot
	wakeCh, cleanup := globalBroker.register(agentDir)
	defer cleanup()

	if testHookPreSnapshotRead != nil {
		testHookPreSnapshotRead()
	}

	// 1. Initial snapshot & replay
	events, baselineSeq, latestSeq, _, err := ReadSessionEventsFromDisk(agentDir)
	if err != nil {
		return err
	}

	var replay []*agentv1.SessionEvent
	var lastEmittedSeq int64
	rewound := false
	rolledBack := false

	if req != nil && req.GetLiveOnly() {
		// Cold start / live only: do not replay history, start streaming from latestSeq
		lastEmittedSeq = latestSeq
	} else if req != nil && req.GetSinceSeq() > 0 {
		since := req.GetSinceSeq()
		if since < baselineSeq {
			rewound = true
			replay = events
		} else if latestSeq < since {
			rewound = true
			rolledBack = true
			replay = events
		} else {
			for _, ev := range events {
				if ev.Seq > since {
					replay = append(replay, ev)
				}
			}
		}
	} else if req != nil && req.GetLastN() > 0 {
		n := int(req.GetLastN())
		if n < len(events) {
			replay = events[len(events)-n:]
		} else {
			replay = events
		}
	} else {
		if baselineSeq > 1 {
			rewound = true
		}
		replay = events
	}

	if rewound {
		if err := stream.Send(&agentv1.SubscribeSessionResponse{Rewound: true, RolledBack: rolledBack}); err != nil {
			return err
		}
	}

	for _, ev := range replay {
		if err := stream.Send(&agentv1.SubscribeSessionResponse{Event: ev}); err != nil {
			return err
		}
		if ev.Seq > lastEmittedSeq {
			lastEmittedSeq = ev.Seq
		}
	}

	// 2. Live streaming loop with bounded buffer and drop-with-notice
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var droppedCount int64

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wakeCh:
		case <-ticker.C:
		}

		// Check for newly committed events on disk
		newEvents, _, _, _, err := ReadSessionEventsFromDisk(agentDir)
		if err != nil {
			continue
		}

		var pending []*agentv1.SessionEvent
		for _, ev := range newEvents {
			if ev.Seq > lastEmittedSeq {
				pending = append(pending, ev)
			}
		}

		if len(pending) > maxSubscriberBuffer {
			dropped := int64(len(pending) - maxSubscriberBuffer)
			droppedCount += dropped
			pending = pending[len(pending)-maxSubscriberBuffer:]
		}

		// Emit drop notice if any were previously dropped
		if droppedCount > 0 {
			if err := stream.Send(&agentv1.SubscribeSessionResponse{DroppedEvents: droppedCount}); err != nil {
				return err
			}
			droppedCount = 0
		}

		for _, ev := range pending {
			if err := stream.Send(&agentv1.SubscribeSessionResponse{Event: ev}); err != nil {
				return err
			}
			lastEmittedSeq = ev.Seq
		}
	}
}
