package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ToolEventStatus is the lifecycle status of a tool invocation for protocol emission.
type ToolEventStatus string

const (
	ToolEventStatusCompleted ToolEventStatus = "completed"
	ToolEventStatusError     ToolEventStatus = "error"
	ToolEventStatusDenied    ToolEventStatus = "denied"
)

// ToolEvent is one tool invocation lifecycle event (announce or outcome). Args and result
// bodies never travel on the wire - only redacted summaries, byte sizes, truncated heads,
// and store references.
type ToolEvent struct {
	CallID      string    `json:"call_id"`
	ToolName    string    `json:"tool_name"`
	ArgsSummary string    `json:"args_summary,omitempty"`
	Status      string    `json:"status"`
	Denied      bool      `json:"denied,omitempty"`
	ResultBytes int64     `json:"result_bytes,omitempty"`
	ResultHead  string    `json:"result_head,omitempty"`
	ResultRef   string    `json:"result_ref,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
}

// ToolEventSink is a thread-safe collector for tool lifecycle events produced by the ADK
// Before/AfterTool callbacks and drained by the SDK stream handlers.
type ToolEventSink struct {
	mu     sync.Mutex
	events []ToolEvent
	// journalPath, when set, appends every completed event to a JSONL tool journal on disk
	// (stream announces; journal preserves the evidence trail).
	journalPath string
}

// NewToolEventSink returns a sink with no journal backing.
func NewToolEventSink() *ToolEventSink {
	return &ToolEventSink{}
}

// NewToolEventSinkWithJournal returns a sink that appends completed/denied events to a
// JSONL journal at the given path (created on first write). Event ordering on the wire is
// unchanged; the journal is a preservation side-effect.
func NewToolEventSinkWithJournal(journalPath string) *ToolEventSink {
	return &ToolEventSink{journalPath: journalPath}
}

// Announce records a tool_call (pre-execution) event. The returned call_id pairs the
// announce with its later Update. Denied announces set Denied and Status=denied (the deny
// path emits announce+update back to back); live announces leave Status empty so consumers
// distinguish tool_call (announce) from tool_call_update (outcome) by Status.
func (s *ToolEventSink) Announce(toolName, argsSummary string, denied bool) string {
	callID := newToolCallID()
	status := ""
	if denied {
		status = string(ToolEventStatusDenied)
	}
	s.record(ToolEvent{CallID: callID, ToolName: toolName, ArgsSummary: argsSummary, Status: status, Denied: denied, Timestamp: time.Now()})
	return callID
}

// Update records a tool_call_update (outcome) event.
func (s *ToolEventSink) Update(callID, toolName, status string, resultBytes int64, resultHead, resultRef string) {
	s.record(ToolEvent{CallID: callID, ToolName: toolName, Status: status, ResultBytes: resultBytes, ResultHead: resultHead, ResultRef: resultRef, Timestamp: time.Now()})
}

// Drain returns and clears all pending events in FIFO order.
func (s *ToolEventSink) Drain() []ToolEvent {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == 0 {
		return nil
	}
	events := s.events
	s.events = nil
	return events
}

func (s *ToolEventSink) record(ev ToolEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	if s.journalPath != "" {
		s.appendJournal(ev)
	}
}

// appendJournal persists the compact form. Best-effort: a failed journal write must never
// fail the turn - the stream already carries the event.
func (s *ToolEventSink) appendJournal(ev ToolEvent) {
	data, err := json.Marshal(ev)
	if err != nil {
		return
	}
	f, err := os.OpenFile(s.journalPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// newToolCallID returns a short random identifier pairing announce with update.
func newToolCallID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// maxArgsSummaryLen caps the redacted scalar args preview.
const maxArgsSummaryLen = 200

// redactSecretShapedValues replaces values whose keys look secret (api key, token, password,
// bearer/authorization headers, secret) with [REDACTED] and flattens non-scalar args into
// a preview string. Never leaks secret-shaped values onto the wire.
func redactSecretShapedValues(args map[string]any) map[string]any {
	out := make(map[string]any, len(args))
	for k, v := range args {
		if isSecretKey(k) {
			out[k] = "[REDACTED]"
			continue
		}
		out[k] = v
	}
	return out
}

// isSecretKey reports whether a map key names a secret-shaped value.
func isSecretKey(key string) bool {
	lower := strings.ToLower(key)
	for _, marker := range []string{"api_key", "apikey", "api-key", "token", "password", "passwd", "secret", "bearer", "authorization", "auth"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// buildArgsSummary renders a redacted scalar preview of tool args, truncated to
// maxArgsSummaryLen bytes. Non-scalar values are replaced with a size marker.
func buildArgsSummary(args map[string]any) string {
	red := redactSecretShapedValues(args)
	var sb strings.Builder
	keys := make([]string, 0, len(red))
	for k := range red {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for i, k := range keys {
		if i > 0 {
			sb.WriteString(", ")
		}
		v := red[k]
		if s, ok := scalarPreview(v); ok {
			fmt.Fprintf(&sb, "%s=%s", k, s)
		} else {
			fmt.Fprintf(&sb, "%s=<%T %d bytes>", k, v, approxSize(v))
		}
	}
	out := sb.String()
	if len(out) > maxArgsSummaryLen {
		out = out[:maxArgsSummaryLen]
	}
	return out
}

// scalarPreview returns a stable string for scalar-like values (strings, numbers, bools).
func scalarPreview(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case float64:
		return fmt.Sprintf("%v", t), true
	case int64:
		return fmt.Sprintf("%d", t), true
	case bool:
		return fmt.Sprintf("%v", t), true
	}
	return "", false
}

// approxSize is a rough byte bound for non-scalar JSON-ish values (marshal + 1KB guard).
func approxSize(v any) int {
	if data, err := json.Marshal(v); err == nil {
		return len(data)
	}
	return 1024
}

// buildResultSummary produces the wire-safe result representation: byte size + truncated
// head (<=256 chars). Full bodies stay out of the stream.
func buildResultSummary(result map[string]any) (bytes int64, head string) {
	data, err := json.Marshal(result)
	if err != nil {
		return 0, ""
	}
	bytes = int64(len(data))
	if len(data) > 256 {
		head = string(data[:256])
	} else {
		head = string(data)
	}
	return
}

// toolJournalPath returns the compact journal path for an agent directory.
func toolJournalPath(agentDir string) string {
	return filepath.Join(agentDir, "tool-journal.jsonl")
}

// toolEventsKey is the context key carrying the per-turn ToolEventSink (D112 tool-call
// visibility). Stream handlers wrap their turn context; impls read it to attach the sink
// to the loaded FolderAgent. Absent = no visibility events (legacy/non-streaming paths).
type toolEventsKeyType struct{}

var toolEventsKey toolEventsKeyType

func withToolEvents(ctx context.Context, sink *ToolEventSink) context.Context {
	if ctx == nil || sink == nil {
		return ctx
	}
	return context.WithValue(ctx, toolEventsKey, sink)
}

func toolEventsFromCtx(ctx context.Context) *ToolEventSink {
	if ctx == nil {
		return nil
	}
	if v, ok := ctx.Value(toolEventsKey).(*ToolEventSink); ok {
		return v
	}
	return nil
}
