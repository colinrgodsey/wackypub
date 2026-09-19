package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	agentv1 "github.com/colinrgodsey/wackypub/pkg/agent/v1"
)

// asideProtoWorkspace builds a workspace with a populated agent (context for the aside)
// whose runtime points at a mock backend returning the given answer, and returns sdk+agentDir.
func asideProtoWorkspace(t *testing.T, answer string) (*AgentSDK, string) {
	t.Helper()
	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	agentID := "apagent"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("test agent"), 0644); err != nil {
		t.Fatalf("AGENTS.md: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, AllowedAgentsFile), []byte(agentID+"\n"), 0644); err != nil {
		t.Fatalf("allowlist: %v", err)
	}
	srv := asideMockServer(t, answer)
	runtimeJSON := fmt.Sprintf(`{"provider":"openai","endpoint":%q,"model":"m","apiKey":"k"}`, srv.URL)
	if err := os.WriteFile(filepath.Join(agentDir, "runtime.json"), []byte(runtimeJSON), 0644); err != nil {
		t.Fatalf("runtime.json: %v", err)
	}
	if err := AppendSessionTurn(agentDir, "user", "question one"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := AppendSessionTurn(agentDir, "model", "answer one"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := WriteMemoryFile(agentDir, "persistent state: proto project"); err != nil {
		t.Fatalf("write memory: %v", err)
	}
	origCwd, _ := os.Getwd()
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCwd) })
	return NewSDK(wsDir), agentDir
}

// TestAsideQuestionProtoRoundTrip validates the protocol surface for a LOCAL agent: the
// unary AsideQuestion RPC returns the contextual answer and the main session stays
// byte-identical, exactly like the SDK path but through agentv1.AgentServiceServer.
func TestAsideQuestionProtoRoundTrip(t *testing.T) {
	sdk, agentDir := asideProtoWorkspace(t, "proto project state is known")

	snapshot := func() map[string]string {
		m := make(map[string]string)
		for _, name := range []string{"session.jsonl", "MEMORY.md", "runtime.json"} {
			if b, err := os.ReadFile(filepath.Join(agentDir, name)); err == nil {
				m[name] = string(b)
			}
		}
		return m
	}
	before := snapshot()

	resp, err := sdk.AsideQuestion(context.Background(), &agentv1.AsideQuestionRequest{
		AgentId:  "apagent",
		Question: "what is the project state?",
	})
	if err != nil {
		t.Fatalf("AsideQuestion: %v", err)
	}
	if resp.GetText() == "" {
		t.Fatal("aside returned empty text")
	}
	if !strings.Contains(resp.GetText(), "proto project state is known") {
		t.Fatalf("aside answer should be contextual, got %q", resp.GetText())
	}

	after := snapshot()
	for name, b := range before {
		if after[name] != b {
			t.Fatalf("%s changed after aside proto call:\nbefore: %q\nafter:  %q", name, b, after[name])
		}
	}
}

// TestAsideQuestionBridgeDispatch validates the REMOTE_MANIFEST bridge path end to end: a
// real bridge subprocess (wackyshimbin, built as a fixture like cmd/remote_dispatch_test.go
// does) serving the generated AgentService over stdio receives the AsideQuestion RPC when
// ResolveAgentClient routes the agent to the bridge. This is the same dispatch seam the
// wackyacp bridge PR will implement server-side for agy/claude.
func TestAsideQuestionBridgeDispatch(t *testing.T) {
	shim := buildShimFixture(t)

	wsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(wsDir, RootMarkerFile), []byte("\n"), 0644); err != nil {
		t.Fatalf("root marker: %v", err)
	}
	agentID := "bridged"
	agentDir := filepath.Join(wsDir, agentID)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("bridged agent"), 0644); err != nil {
		t.Fatalf("AGENTS.md: %v", err)
	}
	manifest := fmt.Sprintf("%s: %s --behavior=echo\n", agentID, shim)
	if err := os.WriteFile(filepath.Join(wsDir, RemoteManifestFile), []byte(manifest), 0644); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	origCwd, _ := os.Getwd()
	if err := os.Chdir(agentDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(origCwd) })

	sdk := NewSDK(wsDir)
	client, cleanup, err := ResolveAgentClient(context.Background(), sdk, agentID)
	if err != nil {
		t.Fatalf("ResolveAgentClient: %v", err)
	}
	defer cleanup()

	resp, err := client.AsideQuestion(context.Background(), &agentv1.AsideQuestionRequest{
		AgentId:  agentID,
		Question: "bridge question",
	})
	if err != nil {
		t.Fatalf("bridged AsideQuestion: %v", err)
	}
	if !strings.Contains(resp.GetText(), "aside from shim: bridge question") {
		t.Fatalf("expected shimbin aside text, got %q", resp.GetText())
	}
	if resp.GetToolDenials() != 3 {
		t.Fatalf("expected shimbin-reported denials to round-trip, got %d", resp.GetToolDenials())
	}
}

// buildShimFixture compiles the wackyshimbin bridge fixture once per test binary (like
// cmd/remote_dispatch_test.go getShim).
var (
	shimBuildOnce sync.Once
	shimPath      string
	shimErr       error
)

func buildShimFixture(t *testing.T) string {
	t.Helper()
	shimBuildOnce.Do(func() {
		tmpDir, err := os.MkdirTemp("", "wackyshimbin-pkg-*")
		if err != nil {
			shimErr = err
			return
		}
		bin := filepath.Join(tmpDir, "wackyshimbin")
		cmd := exec.Command("go", "build", "-o", bin, "github.com/colinrgodsey/wackypub/cmd/internal/wackyshimbin")
		out, err := cmd.CombinedOutput()
		if err != nil {
			shimErr = fmt.Errorf("build wackyshimbin: %w\n%s", err, out)
			return
		}
		shimPath = bin
	})
	if shimErr != nil {
		t.Fatalf("failed to build wackyshimbin fixture: %v", shimErr)
	}
	return shimPath
}
