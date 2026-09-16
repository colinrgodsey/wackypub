package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadRemoteManifest_Absent(t *testing.T) {
	tmpDir := t.TempDir()
	manifest, err := LoadRemoteManifest(tmpDir)
	if err != nil {
		t.Fatalf("expected no error for missing manifest, got: %v", err)
	}
	if manifest.Exists {
		t.Errorf("expected manifest.Exists to be false, got true")
	}
	if len(manifest.Routes) != 0 {
		t.Errorf("expected empty routes, got: %v", manifest.Routes)
	}

	route, ok := manifest.Lookup("any-agent")
	if ok {
		t.Errorf("expected lookup to return false, got true with route: %+v", route)
	}
}

func TestLoadRemoteManifest_ValidRoutes(t *testing.T) {
	tmpDir := t.TempDir()
	content := `
# Comment line
agent1: wackybridge --remote localhost:50051
agent2: /usr/local/bin/wackyacp --timeout "30s" --verbose
agent3: run_agent arg1 "arg 2 with spaces" arg3
`
	manifestPath := filepath.Join(tmpDir, RemoteManifestFile)
	if err := os.WriteFile(manifestPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write manifest: %v", err)
	}

	manifest, err := LoadRemoteManifest(tmpDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if !manifest.Exists {
		t.Fatalf("expected manifest.Exists to be true")
	}
	if len(manifest.Routes) != 3 {
		t.Fatalf("expected 3 routes, got %d", len(manifest.Routes))
	}

	r1, ok := manifest.Lookup("agent1")
	if !ok {
		t.Errorf("expected route for agent1")
	} else {
		expected := RemoteRoute{
			AgentID: "agent1",
			Command: "wackybridge",
			Args:    []string{"--remote", "localhost:50051"},
		}
		if !reflect.DeepEqual(r1, expected) {
			t.Errorf("agent1 route mismatch:\ngot  %+v\nwant %+v", r1, expected)
		}
	}

	r2, ok := manifest.Lookup("agent2")
	if !ok {
		t.Errorf("expected route for agent2")
	} else {
		expected := RemoteRoute{
			AgentID: "agent2",
			Command: "/usr/local/bin/wackyacp",
			Args:    []string{"--timeout", "30s", "--verbose"},
		}
		if !reflect.DeepEqual(r2, expected) {
			t.Errorf("agent2 route mismatch:\ngot  %+v\nwant %+v", r2, expected)
		}
	}

	r3, ok := manifest.Lookup("agent3")
	if !ok {
		t.Errorf("expected route for agent3")
	} else {
		expected := RemoteRoute{
			AgentID: "agent3",
			Command: "run_agent",
			Args:    []string{"arg1", "arg 2 with spaces", "arg3"},
		}
		if !reflect.DeepEqual(r3, expected) {
			t.Errorf("agent3 route mismatch:\ngot  %+v\nwant %+v", r3, expected)
		}
	}

	_, ok = manifest.Lookup("nonexistent")
	if ok {
		t.Errorf("expected nonexistent agent to return false")
	}
}

func TestLoadRemoteManifest_DuplicateAgentID_Error(t *testing.T) {
	tmpDir := t.TempDir()
	content := `
agent1: wackybridge --port 1000
# another line
agent1: wackybridge --port 2000
`
	manifestPath := filepath.Join(tmpDir, RemoteManifestFile)
	if err := os.WriteFile(manifestPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write manifest: %v", err)
	}

	_, err := LoadRemoteManifest(tmpDir)
	if err == nil {
		t.Fatalf("expected error on duplicate route, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate route for agent \"agent1\"") {
		t.Errorf("expected duplicate route error message, got: %v", err)
	}
	if !strings.Contains(err.Error(), ":4:") || !strings.Contains(err.Error(), "line 2") {
		t.Errorf("expected line numbers in error message, got: %v", err)
	}
}

func TestLoadRemoteManifest_MalformedLine_Error(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name:    "missing colon",
			content: "agent1 wackybridge --port 1000",
			wantErr: "missing ':' separator",
		},
		{
			name:    "empty agent_id",
			content: " : wackybridge --port 1000",
			wantErr: "empty agent_id",
		},
		{
			name:    "empty command",
			content: "agent1:   ",
			wantErr: "empty command for agent",
		},
		{
			name:    "unterminated quote",
			content: "agent1: wackybridge \"unterminated quote",
			wantErr: "parsing command for agent",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			manifestPath := filepath.Join(tmpDir, RemoteManifestFile)
			if err := os.WriteFile(manifestPath, []byte(tc.content), 0644); err != nil {
				t.Fatalf("failed to write manifest: %v", err)
			}
			_, err := LoadRemoteManifest(tmpDir)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestLoadRemoteManifest_CommentsAndBlankLines_Ignored(t *testing.T) {
	tmpDir := t.TempDir()
	content := `
# Full line comment
   # Indented comment

   
agent1: bridge_cmd
`
	manifestPath := filepath.Join(tmpDir, RemoteManifestFile)
	if err := os.WriteFile(manifestPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write manifest: %v", err)
	}

	manifest, err := LoadRemoteManifest(tmpDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(manifest.Routes) != 1 {
		t.Fatalf("expected exactly 1 route, got %d", len(manifest.Routes))
	}
	if _, ok := manifest.Lookup("agent1"); !ok {
		t.Errorf("expected route for agent1")
	}
}
