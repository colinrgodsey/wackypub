package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/adk/v2/session"
)

// A file that exists but cannot be read is a different condition from a file that was
// never written. The not-found case stays a valid empty session; a real read failure must
// reach the caller instead of producing a session that looks short but is corrupt.
func TestFileSessionServiceGetSurfacesUnreadableMemory(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	memPath := filepath.Join(agentDir, "MEMORY.md")
	if err := os.WriteFile(memPath, []byte("durable facts"), 0644); err != nil {
		t.Fatalf("write MEMORY.md: %v", err)
	}
	if err := os.Chmod(memPath, 0000); err != nil {
		t.Fatalf("chmod MEMORY.md: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(memPath, 0644) })

	svc := NewFileSessionService(wsDir)
	if _, err := svc.Get(context.Background(), &session.GetRequest{SessionID: "bob"}); err == nil {
		t.Fatal("expected an error for an unreadable MEMORY.md, got nil")
	}
}

func TestFileSessionServiceGetSurfacesUnreadableSessionLog(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	logPath := filepath.Join(agentDir, "session.jsonl")
	if err := os.WriteFile(logPath, []byte("{not json\n"), 0644); err != nil {
		t.Fatalf("write session.jsonl: %v", err)
	}
	if err := os.Chmod(logPath, 0000); err != nil {
		t.Fatalf("chmod session.jsonl: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(logPath, 0644) })

	svc := NewFileSessionService(wsDir)
	if _, err := svc.Get(context.Background(), &session.GetRequest{SessionID: "bob"}); err == nil {
		t.Fatal("expected an error for an unreadable session.jsonl, got nil")
	}
}

func TestFileSessionServiceGetMissingFilesStayNonError(t *testing.T) {
	wsDir := t.TempDir()
	agentDir := filepath.Join(wsDir, "bob")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	svc := NewFileSessionService(wsDir)
	resp, err := svc.Get(context.Background(), &session.GetRequest{SessionID: "bob"})
	if err != nil {
		t.Fatalf("a never-written MEMORY.md or session.jsonl must not be an error, got %v", err)
	}
	if resp == nil || resp.Session == nil {
		t.Fatal("expected a session for an agent with no persisted files")
	}
}
