package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	adkAgent "github.com/colinrgodsey/wackypub/pkg/agent"
)

// TestRemoteDispatch_ConcurrentSameAgentSerializes reproduces
// bugs/wackypub/bridge-concurrent-prompt-lock at the dispatch layer: two concurrent
// RPCs to the SAME bridged agent must serialize (one bridge process at a time), never
// racing two bridge processes against one agent session. The shim is started with
// --guard-lock so a second concurrent bridge process exits 1 - without the dispatch-
// side serialization this surfaces as a died-bridge/unclosed-stream failure on one of
// the two calls.
func TestRemoteDispatch_ConcurrentSameAgentSerializes(t *testing.T) {
	wsDir := t.TempDir()
	shim := getShim(t)
	guardFile := filepath.Join(wsDir, "bridge.guard")

	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RootMarkerFile), []byte(""), 0644); err != nil {
		t.Fatalf("write root marker: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.AllowedAgentsFile), []byte("bridgedagent\n"), 0644); err != nil {
		t.Fatalf("write allowlist: %v", err)
	}
	manifestContent := fmt.Sprintf("bridgedagent: %s --behavior=aside-slow --guard-lock %s\n", shim, guardFile)
	if err := os.WriteFile(filepath.Join(wsDir, adkAgent.RemoteManifestFile), []byte(manifestContent), 0644); err != nil {
		t.Fatalf("write REMOTE_MANIFEST: %v", err)
	}

	origCwd, _ := os.Getwd()
	if err := os.Chdir(wsDir); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer os.Chdir(origCwd)

	sdk := adkAgent.NewSDK(wsDir)

	// Fire two concurrent AsideQuestion RPCs at the same agent from a barrier. With the
	// per-agent bridge lock they serialize; without it the guard-lock shim would crash
	// the second process and one call fails with a bridge-death error.
	const n = 2
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			text, err := asideQuestionProto(sdk, context.Background(), "bridgedagent", "q", nil)
			errs[idx] = err
			if err == nil {
				if !strings.Contains(text, "aside from shim: q") {
					errs[idx] = fmt.Errorf("unexpected aside text: %q", text)
				}
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("dispatch %d failed: %v", i, errs[i])
		}
	}
}
