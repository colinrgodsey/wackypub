package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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
	// notify is a buffered(1) push signal fired on every record so stream handlers can wake
	// and drain mid-tool (liveness): the announce becomes observable before tool completion.
	notify chan struct{}
}

// NewToolEventSink returns a sink with no journal backing.
func NewToolEventSink() *ToolEventSink {
	return &ToolEventSink{notify: make(chan struct{}, 1)}
}

// NewToolEventSinkWithJournal returns a sink that appends completed/denied events to a
// JSONL journal at the given path (created on first write). Event ordering on the wire is
// unchanged; the journal is a preservation side-effect.
func NewToolEventSinkWithJournal(journalPath string) *ToolEventSink {
	return &ToolEventSink{journalPath: journalPath, notify: make(chan struct{}, 1)}
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

// Notify returns a push channel signaled (buffered, at least once) whenever a new tool
// event is recorded. Stream handlers select on it to drain between text chunks AND while
// a tool is still executing (the announce becomes live, not deferred to tool completion).
func (s *ToolEventSink) Notify() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.notify
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
	select {
	case s.notify <- struct{}{}:
	default:
	}
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

// secretKeyRe matches a secret-shaped KEY as a whole token (with word boundaries), so
// common substring false positives (author, authorize, tokenizer, authority) do not render
// as [REDACTED] - only keys that ARE secret-shaped (api_key, access_token, bearer,
// authorization, auth, password, secret, token...).
var secretKeyRe = regexp.MustCompile(`(?i)(^|[^a-z0-9_])(api[_-]?key|apikey|access[_-]?token|bearer[_-]?token|authtoken|token|password|passwd|secret|bearer|authorization|auth)([^a-z0-9_]|$)`)

// isSecretKey reports whether a map key names a secret-shaped value.
func isSecretKey(key string) bool {
	return secretKeyRe.MatchString(key)
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
			// B2: secret-shaped values inside non-secret keys (e.g. command=curl -H Authorization:
			// Bearer sk-live-...) must be redacted at the VALUE level too.
			fmt.Fprintf(&sb, "%s=%s", k, redactSecretValues(s))
		} else {
			fmt.Fprintf(&sb, "%s=<%T %d bytes>", k, v, approxSize(v))
		}
	}
	// B1: rune-safe truncation - byte-slicing mid-rune makes the string invalid UTF-8 and proto
	// refuses to marshal it, killing the whole turn.
	return truncateRunesSafe(sb.String(), maxArgsSummaryLen)
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
// head (<=256 bytes) + value-level redaction (B3: tool results routinely carry file contents
// with OPENAI_API_KEY=sk-... style lines - the head must not leak them). Full bodies stay
// out of the stream.
func buildResultSummary(result map[string]any) (bytes int64, head string) {
	data, err := json.Marshal(result)
	if err != nil {
		return 0, ""
	}
	bytes = int64(len(data))
	// Redact BEFORE truncating: a secret straddling the 256-byte cut (e.g. sk-live-... with
	// the token starting at byte ~240) would otherwise survive as a stub too short for the
	// value patterns to match. Redact the full body, then take the head.
	redacted := redactSecretValues(string(data))
	head = truncateRunesSafe(redacted, 256)
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

// truncateRunesSafe truncates s to at most max bytes without splitting a UTF-8 rune.
// Byte-slicing can land mid-rune, and proto (protobuf string fields must be valid UTF-8)
// then REFUSES to marshal the response - which would fail the whole turn, not just the
// preview. Same approach agy bridge uses (truncateRunes) for consistency.
func truncateRunesSafe(s string, max int) string {
	if len(s) <= max {
		return s
	}
	out := s[:max]
	for !utf8.ValidString(out) {
		out = out[:len(out)-1]
	}
	return out
}

// secretValuePatterns match secret-shaped substrings inside arbitrary values (B2/B3): a
// token embedded in a non-secret keyed value (e.g. command=curl -H Authorization: Bearer
// sk-live-...) must still be removed from the wire. Redaction is value-level, not just
// key-level.
var secretValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`),                                             // OpenAI-style key
	regexp.MustCompile(`sk_live_[A-Za-z0-9_-]{8,}`),                                        // Stripe-style key
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                                                 // AWS access key ID
	regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`),                                       // GitHub PAT / OAuth / refresh / server-to-server / user tokens
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),                                            // Google API key
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`),                                     // Slack tokens (bot, app, app-level, user)
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), // bare JWT (no Bearer prefix)
	regexp.MustCompile(`Bearer\s+[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`Basic\s+[A-Za-z0-9+/=]+`),
	regexp.MustCompile(`-----BEGIN [A-Z ]+-----`),
	regexp.MustCompile(`(?i)(api[_-]?key|token|password|passwd|secret|bearer|authorization|aws_access_key_id|secret_access_key)[:=]\s*[^\s,\};]+`),
}

// redactSecretValues scans a string for secret-shaped substrings and replaces them with
// [REDACTED]. Applies to scalar arg VALUES and result heads, not just secret-named keys.
func redactSecretValues(s string) string {
	for _, re := range secretValuePatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}
