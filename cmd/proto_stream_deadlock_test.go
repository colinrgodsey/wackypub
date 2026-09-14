package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

// TestProtoStreamHelperTerminates_AddAndGenerate is the regression guard for the D112
// Phase 2 CLI deadlock: generateTurnStreamProto and addAndGenerateTurnStreamProto used to
// defer stream.Close() in the ranging closure, which could never fire because the range
// waits on a channel nobody closes. The producer goroutine now owns Close. If the bug
// regresses, this test hangs past the 10s guard and fails on timeout.
func TestProtoStreamHelperTerminates_AddAndGenerate(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "streamagent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("test agent"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, adkAgent.AllowedAgentsFile), []byte(agentID+"\n"), 0644); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("write root marker: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-test","object":"chat.completion","created":123,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	runtimeJSON := fmt.Sprintf(`{"model":"test-model","endpoint":%q}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("write runtime.json: %v", err)
	}

	origCwd, _ := os.Getwd()
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	sdk := adkAgent.NewSDK(wsDir)
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var got []string
		for text, err := range addAndGenerateTurnStreamProto(sdk, ctx, agentID, "hello", nil) {
			if err != nil {
				t.Errorf("stream error: %v", err)
				return
			}
			got = append(got, text)
		}
		if len(got) == 0 {
			t.Errorf("expected streamed text, got none")
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("addAndGenerateTurnStreamProto deadlocked: range did not terminate after a successful turn")
	}
}

// TestProtoStreamHelperTerminates_Generate mirrors the same regression guard for the
// continue-only streaming helper.
func TestProtoStreamHelperTerminates_Generate(t *testing.T) {
	wsDir := t.TempDir()
	agentID := "streamgen"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("test agent"), 0644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, adkAgent.AllowedAgentsFile), []byte(agentID+"\n"), 0644); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("write root marker: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-test","object":"chat.completion","created":123,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	runtimeJSON := fmt.Sprintf(`{"model":"test-model","endpoint":%q}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("write runtime.json: %v", err)
	}
	if err := adkAgent.AppendSessionTurn(agentDir, "user", "hello"); err != nil {
		t.Fatalf("append session: %v", err)
	}

	origCwd, _ := os.Getwd()
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	sdk := adkAgent.NewSDK(wsDir)
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var got []string
		for text, err := range generateTurnStreamProto(sdk, ctx, agentID) {
			if err != nil {
				t.Errorf("stream error: %v", err)
				return
			}
			got = append(got, text)
		}
		if len(got) == 0 {
			t.Errorf("expected streamed text, got none")
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("generateTurnStreamProto deadlocked: range did not terminate after a successful turn")
	}
}
