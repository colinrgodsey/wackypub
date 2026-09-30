package agent

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

type FileSessionService struct {
	wsDir string
}

func NewFileSessionService(wsDir string) *FileSessionService {
	return &FileSessionService{wsDir: wsDir}
}

type fileSessionState struct {
	data map[string]any
}

func (s *fileSessionState) Get(key string) (any, error) {
	v, ok := s.data[key]
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return v, nil
}

func (s *fileSessionState) Set(key string, val any) error {
	if s.data == nil {
		s.data = make(map[string]any)
	}
	s.data[key] = val
	return nil
}

func (s *fileSessionState) Delete(key string) {
	delete(s.data, key)
}

func (s *fileSessionState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for k, v := range s.data {
			if !yield(k, v) {
				return
			}
		}
	}
}

type eventList struct {
	events []*session.Event
}

func (el *eventList) All() iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		for _, e := range el.events {
			if !yield(e) {
				return
			}
		}
	}
}

func (el *eventList) Len() int {
	return len(el.events)
}

func (el *eventList) At(i int) *session.Event {
	return el.events[i]
}

type fileSession struct {
	id         string
	appName    string
	userID     string
	state      session.State
	events     session.Events
	updateTime time.Time
}

func (fs *fileSession) ID() string                { return fs.id }
func (fs *fileSession) AppName() string           { return fs.appName }
func (fs *fileSession) UserID() string            { return fs.userID }
func (fs *fileSession) State() session.State      { return fs.state }
func (fs *fileSession) Events() session.Events    { return fs.events }
func (fs *fileSession) LastUpdateTime() time.Time { return fs.updateTime }

func (s *FileSessionService) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	agentID := req.SessionID
	if agentID == "" {
		return nil, fmt.Errorf("session ID (agent ID) is required")
	}

	agentDir := filepath.Join(s.wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create agent dir %s: %w", agentDir, err)
	}

	fs := &fileSession{
		id:         agentID,
		appName:    req.AppName,
		userID:     req.UserID,
		state:      &fileSessionState{data: req.State},
		events:     &eventList{},
		updateTime: time.Now(),
	}
	return &session.CreateResponse{Session: fs}, nil
}

func (s *FileSessionService) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	agentID := req.SessionID
	if agentID == "" {
		return nil, fmt.Errorf("session ID (agent ID) is required")
	}

	agentDir := filepath.Join(s.wsDir, agentID)

	// 1. Read MEMORY.md and format persistent memory turn
	memContent, err := ReadMemoryFile(agentDir)
	if err != nil {
		return nil, err
	}
	memTurnText := FormatPersistentMemoryTurn(memContent)
	memContentTurn := genai.NewContentFromText(memTurnText, "user")

	// 2. Read turns from session.jsonl
	sessionTurns, err := ReadSessionTurns(agentDir)
	if err != nil {
		return nil, err
	}

	// Clean and normalize session turns (strip dangling function responses, prune empty turns, merge consecutive user turns)
	var rawContents []*genai.Content
	rawContents = append(rawContents, memContentTurn)
	rawContents = append(rawContents, sessionTurns...)
	mergedContents := CleanSessionTurns(rawContents)

	var evts []*session.Event
	for i, c := range mergedContents {
		evt := session.NewEvent(ctx, fmt.Sprintf("turn_%d", i))
		evt.Content = c
		if c.Role == "model" {
			evt.Author = agentID
		} else {
			evt.Author = "user"
		}
		evts = append(evts, evt)
	}

	fs := &fileSession{
		id:         agentID,
		appName:    req.AppName,
		userID:     req.UserID,
		state:      &fileSessionState{},
		events:     &eventList{events: evts},
		updateTime: time.Now(),
	}

	return &session.GetResponse{Session: fs}, nil
}

func (s *FileSessionService) List(ctx context.Context, req *session.ListRequest) (*session.ListResponse, error) {
	ids, err := ListAgentIDs(s.wsDir)
	if err != nil {
		return nil, err
	}

	var sessions []session.Session
	for _, id := range ids {
		sessions = append(sessions, &fileSession{
			id:         id,
			appName:    req.AppName,
			userID:     req.UserID,
			state:      &fileSessionState{},
			events:     &eventList{},
			updateTime: time.Now(),
		})
	}
	return &session.ListResponse{Sessions: sessions}, nil
}

func (s *FileSessionService) Delete(ctx context.Context, req *session.DeleteRequest) error {
	agentDir := filepath.Join(s.wsDir, req.SessionID)
	sessionPath := filepath.Join(agentDir, "session.jsonl")
	if pathExists(sessionPath) {
		return os.Remove(sessionPath)
	}
	return nil
}

func (s *FileSessionService) AppendEvent(ctx context.Context, sess session.Session, evt *session.Event) error {
	if evt == nil || evt.Content == nil {
		return nil
	}

	// Update in-memory session event list so subsequent iterations inside the runner tool loop see updated events
	if fs, ok := sess.(*fileSession); ok && fs.events != nil {
		if el, ok := fs.events.(*eventList); ok {
			el.events = append(el.events, evt)
		}
	}

	agentID := sess.ID()
	agentDir := filepath.Join(s.wsDir, agentID)

	// Check if this is the first system/memory turn prefix (do not re-persist it to session.jsonl)
	if len(evt.Content.Parts) > 0 && evt.Content.Parts[0].Text != "" {
		if strings.Contains(evt.Content.Parts[0].Text, "<PERSISTENT_MEMORY>") {
			return nil
		}
	}

	return AppendSessionContent(agentDir, evt.Content)
}
