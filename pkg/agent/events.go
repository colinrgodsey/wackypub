package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

var subscribeTickerInterval = 50 * time.Millisecond

const maxSubscriberBuffer = 64

// testHookPreSnapshotRead is invoked immediately after broker registration and before reading the initial snapshot.
// Used in tests to verify that broker registration before snapshot read eliminates the race window.
var testHookPreSnapshotRead func()

// testHookSessionDiskRead is invoked whenever session.jsonl is opened from disk (either full
// read or incremental read). Used in regression tests to verify that idle watch sessions
// produce zero read activity.
var testHookSessionDiskRead func()

func parseSessionEventFromLine(lineBytes []byte, fallbackSeq int64) (*agentv1.SessionEvent, bool) {
	if len(lineBytes) == 0 {
		return nil, false
	}
	rawLine := string(lineBytes)

	var pt PersistedTurn
	if err := json.Unmarshal(lineBytes, &pt); err != nil {
		return nil, false
	}

	seq := pt.Seq
	if seq <= 0 {
		seq = fallbackSeq
	}

	st := &agentv1.SessionTurn{
		Role: pt.Role,
		Seq:  seq,
	}
	if contentJSON, err := json.Marshal(&pt.Content); err == nil {
		st.ContentJson = string(contentJSON)
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
			compactionText = p.Text
		}
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
	return sev, isCompaction
}

// readSessionEventsIncremental reads newly appended lines from session.jsonl starting from startOffset.
func readSessionEventsIncremental(sessionPath string, startOffset int64, lastEmittedSeq int64) ([]*agentv1.SessionEvent, int64, error) {
	f, err := os.Open(sessionPath)
	if err != nil {
		return nil, startOffset, err
	}
	defer f.Close()

	if testHookSessionDiskRead != nil {
		testHookSessionDiskRead()
	}

	if startOffset > 0 {
		if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
			return nil, startOffset, fmt.Errorf("seeking to %d in %s: %w", startOffset, SessionFileName, err)
		}
	}

	reader := bufio.NewReaderSize(f, 64*1024)
	offset := startOffset
	var events []*agentv1.SessionEvent
	fallbackSeq := lastEmittedSeq

	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if line[len(line)-1] != '\n' {
				// Incomplete line write in progress! Do not advance offset past it.
				// Next poll/notify tick will read the complete line once flushed with \n.
				break
			}
			if len(line) > MaxSessionLineBytes {
				return events, offset, fmt.Errorf("line exceeds maximum length of %d bytes", MaxSessionLineBytes)
			}
			offset += int64(len(line))
			trimmed := bytes.TrimRight(line, "\r\n")
			fallbackSeq++
			sev, _ := parseSessionEventFromLine(trimmed, fallbackSeq)
			if sev != nil {
				if sev.Seq > fallbackSeq {
					fallbackSeq = sev.Seq
				}
				events = append(events, sev)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return events, offset, readErr
		}
	}

	return events, offset, nil
}

// ReadSessionEventsFromDisk reads the durable event stream from session.jsonl, the only
// durable source. A tool attempt that never reached a persisted turn is not in it and
// reaches consumers live instead.
func ReadSessionEventsFromDisk(agentDir string) ([]*agentv1.SessionEvent, int64, int64, int64, error) {
	var events []*agentv1.SessionEvent
	var latestTurnSeq int64
	var sessionBaselineSeq int64
	var hasCompaction bool

	// 1. Read session.jsonl
	sessionPath := filepath.Join(agentDir, SessionFileName)
	if f, err := os.Open(sessionPath); err == nil {
		if testHookSessionDiskRead != nil {
			testHookSessionDiskRead()
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 1024*1024), MaxSessionLineBytes)
		var lineIdx int64
		for scanner.Scan() {
			lineIdx++
			lineBytes := scanner.Bytes()
			sev, isCompaction := parseSessionEventFromLine(lineBytes, lineIdx)
			if sev == nil {
				continue
			}
			seq := sev.Seq
			if isCompaction {
				hasCompaction = true
				if sessionBaselineSeq == 0 {
					sessionBaselineSeq = seq
				}
			} else {
				if sessionBaselineSeq == 0 || seq < sessionBaselineSeq {
					sessionBaselineSeq = seq
				}
				if seq > latestTurnSeq {
					latestTurnSeq = seq
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

	// Sort strictly by sequence number
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Seq < events[j].Seq
	})

	var baselineSeq int64
	var latestSeq int64
	if hasCompaction && sessionBaselineSeq > 0 {
		// The session baseline defines the rewind boundary: events below it belonged to
		// turns a consumer can no longer see, so they are dropped rather than replayed.
		filtered := events[:0]
		for _, ev := range events {
			if ev.Seq >= sessionBaselineSeq {
				filtered = append(filtered, ev)
			}
		}
		events = filtered
		baselineSeq = sessionBaselineSeq
	} else {
		for _, ev := range events {
			if ev.Seq > 0 {
				baselineSeq = ev.Seq
				break
			}
		}
	}
	if len(events) > 0 {
		latestSeq = events[len(events)-1].Seq
	}
	// no readable seq file simply means nothing beyond the event list to account for.
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
		return nil, errAgentIDRequired
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

	if req != nil && req.GetFromSeq() > 0 {
		from := req.GetFromSeq()
		if baselineSeq > 0 && from < baselineSeq {
			rewound = true
			filtered = events
		} else if latestSeq < from-1 {
			// Rollback or restored directory: client cursor is ahead of current session head!
			rewound = true
			rolledBack = true
			filtered = events
		} else {
			for _, ev := range events {
				if ev.Seq >= from {
					filtered = append(filtered, ev)
				}
			}
		}
	} else if req != nil && req.GetSinceSeq() > 0 {
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
func (s *AgentSDK) SubscribeSession(req *agentv1.SubscribeSessionRequest, stream grpc.ServerStreamingServer[agentv1.SubscribeSessionResponse]) (err error) {
	wsDir := s.WorkspaceDir
	if req != nil && req.GetWorkspaceDir() != "" {
		wsDir = req.GetWorkspaceDir()
	}
	agentID := ""
	if req != nil {
		agentID = req.GetAgentId()
	}

	defer func() {
		if r := recover(); r != nil {
			if IsUnrecoverable(r) {
				fmt.Fprintf(os.Stderr, "wackypub: fatal unrecoverable panic in SubscribeSession for agent %s: %v\n", agentID, r)
				panic(r)
			}
			stack := debug.Stack()
			fmt.Fprintf(os.Stderr, "wackypub: recovered panic in SubscribeSession for agent %s: %v\n%s\n", agentID, r, stack)
			err = status.Errorf(codes.Internal, "panic in SubscribeSession: %v", r)
		}
	}()

	if agentID == "" {
		return errAgentIDRequired
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

	sessionPath := filepath.Join(agentDir, SessionFileName)
	var lastFileInfo os.FileInfo
	var lastOffset int64
	var lastFileExists bool
	if fi, err := os.Stat(sessionPath); err == nil {
		lastFileInfo = fi
		lastOffset = fi.Size()
		lastFileExists = true
	}

	// 2. Live streaming loop with bounded buffer
	ticker := time.NewTicker(subscribeTickerInterval)
	defer ticker.Stop()

	var pending []*agentv1.SessionEvent
	var hasMore bool

	for {
		if !hasMore {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-wakeCh:
			case <-ticker.C:
			}
		}
		hasMore = false

		if len(pending) == 0 {
			fi, err := os.Stat(sessionPath)
			if err != nil {
				if os.IsNotExist(err) {
					if lastFileExists {
						// File was removed
						lastFileExists = false
						lastFileInfo = nil
						lastOffset = 0
					}
					continue
				}
				// Stat error other than NotExist: back off to avoid spinning
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(50 * time.Millisecond):
				}
				continue
			}

			// File exists. Check if it changed since last read.
			if lastFileExists && lastFileInfo != nil && os.SameFile(lastFileInfo, fi) &&
				fi.Size() == lastFileInfo.Size() && fi.ModTime().Equal(lastFileInfo.ModTime()) {
				// Watched session is idle: zero read activity required.
				continue
			}

			var newEvents []*agentv1.SessionEvent
			if lastFileExists && lastFileInfo != nil && os.SameFile(lastFileInfo, fi) && fi.Size() >= lastOffset {
				// Incremental append path
				readEvs, newOff, readErr := readSessionEventsIncremental(sessionPath, lastOffset, lastEmittedSeq)
				if readErr != nil {
					// Fallback to full re-read if incremental read failed
					newEvents, _, _, _, err = ReadSessionEventsFromDisk(agentDir)
					if err != nil {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-time.After(50 * time.Millisecond):
						}
						continue
					}
					lastOffset = fi.Size()
				} else {
					newEvents = readEvs
					lastOffset = newOff
				}
			} else {
				// File was created, replaced (compaction rename), or truncated
				newEvents, _, _, _, err = ReadSessionEventsFromDisk(agentDir)
				if err != nil {
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(50 * time.Millisecond):
					}
					continue
				}
				lastOffset = fi.Size()
			}

			lastFileInfo = fi
			lastFileExists = true

			for _, ev := range newEvents {
				if ev.Seq > lastEmittedSeq {
					pending = append(pending, ev)
				}
			}
		}

		if len(pending) > 0 {
			batchSize := len(pending)
			if batchSize > maxSubscriberBuffer {
				batchSize = maxSubscriberBuffer
			}
			batch := pending[:batchSize]
			pending = pending[batchSize:]

			for _, ev := range batch {
				if err := stream.Send(&agentv1.SubscribeSessionResponse{Event: ev}); err != nil {
					return err
				}
				if ev.Seq > lastEmittedSeq {
					lastEmittedSeq = ev.Seq
				}
			}
		}

		if len(pending) > 0 {
			hasMore = true
		}
	}
}
