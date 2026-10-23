package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
)

// Acceptance tests for tasks/wackypub/stdio-serve-holds-session-lock-while-idle: a
// long-lived serve process must not pin the session lock while idle - the lock is
// scoped to the turn, so a concurrent dispatcher takes it the moment a turn ends, and
// the watchdog's terminal path releases it too.

func driveTurn(t *testing.T, client agentv1.AgentServiceClient, agentID, msg string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.AddAndGenerateTurnStream(ctx, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     agentID,
		UserMessage: msg,
	})
	if err != nil {
		t.Fatalf("open turn stream: %v", err)
	}
	for {
		if _, err := stream.Recv(); err != nil {
			if err == io.EOF {
				return
			}
			t.Fatalf("turn stream for %q: %v", msg, err)
		}
	}
}

// Acceptance: idle server holds no lock. After a completed turn the server is parked
// between RPCs, and a concurrent dispatcher must acquire the session lock immediately.
func TestStdioServe_IdleServerHoldsNoLock(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	agentDir := createTestAgentWithMockRuntime(t, wsDir, "idleagent", "hi")

	client, cleanup := startTestRoutingClient(t, wsDir)
	defer cleanup()

	driveTurn(t, client, "idleagent", "hello")

	ackCtx, ackCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ackCancel()
	lock, err := AcquireSessionLockContext(ackCtx, agentDir)
	if err != nil {
		t.Fatalf("an idle server still holds the session lock: %v", err)
	}
	lock.Release()
}

// Acceptance: a concurrent second dispatcher can drive a full turn while the first is
// idle - the second turn's own lock acquisition is the proof.
func TestStdioServe_SecondDispatcherTurnsWhileFirstIdle(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	agentDir := createTestAgentWithMockRuntime(t, wsDir, "twodispatchers", "hi")

	client, cleanup := startTestRoutingClient(t, wsDir)
	defer cleanup()

	driveTurn(t, client, "twodispatchers", "first dispatcher")
	driveTurn(t, client, "twodispatchers", "second dispatcher")

	// And the lock is free again once the second turn is done.
	ackCtx, ackCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ackCancel()
	lock, err := AcquireSessionLockContext(ackCtx, agentDir)
	if err != nil {
		t.Fatalf("the lock must be free after both turns: %v", err)
	}
	lock.Release()
}

// Acceptance: the lock is released after a turn on the watchdog's terminal path. A
// backend that accepts the request and never answers cannot terminate its stream on its
// own; only the turn watchdog can end the level - and ending it must free the lock.
func TestStdioServe_LockReleasedAfterWatchdogFire(t *testing.T) {
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	agentDir := createTestAgentWithMockRuntime(t, wsDir, "wedgeagent", "hi")

	release := make(chan struct{})
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer func() {
		close(release)
		hang.Close()
	}()
	runtimeJSON := fmt.Sprintf(`{"provider":"openai","endpoint":%q,"model":"m","apiKey":"k","turnDeadlineSeconds":2}`, hang.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("runtime.json: %v", err)
	}

	client, cleanup := startTestRoutingClient(t, wsDir)
	defer cleanup()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := client.AddAndGenerateTurnStream(ctx, &agentv1.AddAndGenerateTurnStreamRequest{
		AgentId:     "wedgeagent",
		UserMessage: "wedge me",
	})
	if err != nil {
		t.Fatalf("open turn stream: %v", err)
	}
	sawTerminal := false
	for {
		if _, err := stream.Recv(); err != nil {
			if strings.Contains(err.Error(), "watchdog") {
				sawTerminal = true
			}
			break
		}
	}
	elapsed := time.Since(start)
	if elapsed < time.Second {
		t.Fatalf("the turn ended before the watchdog could have fired (%v); the RPC failed for another reason", elapsed)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("the watchdog did not bound the wedge: the turn RPC ran %v", elapsed)
	}
	if !sawTerminal {
		t.Error("the watchdog's named terminal state must reach the dispatcher")
	}

	// The acceptance: the lock is free the moment the terminal path completes.
	ackCtx, ackCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ackCancel()
	lock, err := AcquireSessionLockContext(ackCtx, agentDir)
	if err != nil {
		t.Fatalf("the lock must be released on the watchdog's terminal path: %v", err)
	}
	lock.Release()
}
