package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The erase-context example is the documented way to run an always-fresh agent, so its
// guarantees are tested rather than assumed: in-place truncation, lock and note preservation,
// the opt-in gate, the dry run, and degrade-not-wedge on garbage.

func eraseScript(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "examples", "hooks", "turn-completed", "erase-context"))
	if err != nil {
		t.Fatalf("resolve script path: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Skipf("NOT COVERED IN THIS RUN: %s is missing, so the always-fresh example was never exercised", path)
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("NOT COVERED IN THIS RUN: python3 is unavailable, and the example needs it to read its payload")
	}
	return path
}

// eraseFixture builds an agent folder with turn state and returns it plus the inodes that must
// survive an erase.
func eraseFixture(t *testing.T, runtimeJSON string) (dir string, sessionIno, lockIno uint64) {
	t.Helper()
	dir = t.TempDir()
	session := filepath.Join(dir, "session.jsonl")
	if err := os.WriteFile(session, []byte(`{"role":"user","text":"one"}`+"\n"+`{"role":"model","text":"two"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(dir, "session.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("the summary the turn just compacted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".wackyacp-prompt-state.json"), []byte(`{"session_id":"abc"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "runtime.json"), []byte(runtimeJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, inodeOf(t, session), inodeOf(t, lock)
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("NOT COVERED IN THIS RUN: inode identity is unavailable on this platform")
	}
	return stat.Ino
}

func runErase(t *testing.T, script, agentDir, payload string, env ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", script)
	cmd.Dir = agentDir
	cmd.Stdin = strings.NewReader(payload)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func completedPayload(agentDir, reason string) string {
	return fmt.Sprintf(`{"agent_id":"fresh","agent_dir":%q,"event":"turn-completed","reason":%q,"duration_ms":12}`, agentDir, reason)
}

func TestEraseContextExampleTruncatesInPlaceAndPreservesTheLock(t *testing.T) {
	script := eraseScript(t)
	dir, sessionIno, lockIno := eraseFixture(t, `{"provider":"openai","eraseAfterTurn":true}`)

	out, err := runErase(t, script, dir, completedPayload(dir, "completed"))
	if err != nil {
		t.Fatalf("erase failed: %v\n%s", err, out)
	}

	session := filepath.Join(dir, "session.jsonl")
	info, err := os.Stat(session)
	if err != nil {
		t.Fatalf("session.jsonl must still exist after an erase: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("session.jsonl should be truncated, size=%d", info.Size())
	}
	if got := inodeOf(t, session); got != sessionIno {
		t.Fatalf("session.jsonl was replaced rather than truncated (ino %d -> %d): a new inode breaks seq and watcher identity", sessionIno, got)
	}
	if got := inodeOf(t, filepath.Join(dir, "session.lock")); got != lockIno {
		t.Fatalf("session.lock was replaced rather than left alone (ino %d -> %d): unlinking the lock file would split-brain the session lock", lockIno, got)
	}

	kept, err := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if err != nil || string(kept) != "the summary the turn just compacted" {
		t.Fatalf("MEMORY.md must survive an erase: %q %v", kept, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".wackyacp-prompt-state.json")); !os.IsNotExist(err) {
		t.Fatal("the resume linkage must be cleared, or the next turn resumes the erased context from the SDK store")
	}

	audit, err := os.ReadFile(filepath.Join(dir, "context-erase.log"))
	if err != nil {
		t.Fatalf("the erase must leave an audit line so it can be told apart from a crash: %v", err)
	}
	for _, want := range []string{"reason=completed", "rows=2", "bytes=", "prompt_state=removed"} {
		if !strings.Contains(string(audit), want) {
			t.Fatalf("audit line missing %s: %s", want, audit)
		}
	}
	if !strings.Contains(out, "erased context after a completed turn") {
		t.Fatalf("the counts should also be loud on stderr: %q", out)
	}
}

// Installing the hook is the whole opt-in. There is deliberately no config gate, so this asserts
// that the erase happens with no runtime.json present at all: this project does not hand-hold, and
// dropping the script into an agent's hook directory is the decision.
func TestEraseContextExampleNeedsNoConfigGate(t *testing.T) {
	script := eraseScript(t)
	dir, _, _ := eraseFixture(t, `{"provider":"openai"}`)
	if err := os.Remove(filepath.Join(dir, "runtime.json")); err != nil {
		t.Fatalf("remove runtime.json: %v", err)
	}

	out, err := runErase(t, script, dir, completedPayload(dir, "completed"))
	if err != nil {
		t.Fatalf("erase must run without any runtime config: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(dir, "session.jsonl"))
	if err != nil {
		t.Fatalf("session.jsonl should still exist: %v", err)
	}
	if len(body) != 0 {
		t.Fatalf("the erase should proceed with no config present, session size=%d", len(body))
	}
}

func TestEraseContextExampleDryRunChangesNothing(t *testing.T) {
	script := eraseScript(t)
	dir, _, _ := eraseFixture(t, `{"provider":"openai","eraseAfterTurn":true}`)

	out, err := runErase(t, script, dir, completedPayload(dir, "completed"), "WACKYPUB_ERASE_DRY_RUN=1")
	if err != nil {
		t.Fatalf("dry run must exit 0: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(dir, "session.jsonl"))
	if err != nil || !strings.Contains(string(body), "one") {
		t.Fatalf("a dry run must not touch the session: %q %v", body, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".wackyacp-prompt-state.json")); err != nil {
		t.Fatal("a dry run must not remove the resume state either")
	}
	if !strings.Contains(out, "dry run") || !strings.Contains(out, "2 rows") {
		t.Fatalf("a dry run should say what it would do: %q", out)
	}
}

// The script lives on turn-completed, and defensively ignores any other reason it might be
// handed, so a failed turn can never be erased by it.
func TestEraseContextExampleLeavesAFailedTurnAlone(t *testing.T) {
	script := eraseScript(t)
	dir, _, _ := eraseFixture(t, `{"provider":"openai","eraseAfterTurn":true}`)

	out, err := runErase(t, script, dir, completedPayload(dir, "stream_error"))
	if err != nil {
		t.Fatalf("declining to erase must exit 0: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(dir, "session.jsonl"))
	if err != nil || !strings.Contains(string(body), "one") {
		t.Fatalf("a failed turn's context must survive: %q %v", body, err)
	}
}

func TestEraseContextExampleGarbagePayloadDegradesQuietly(t *testing.T) {
	script := eraseScript(t)
	dir, _, _ := eraseFixture(t, `{"provider":"openai","eraseAfterTurn":true}`)

	for _, payload := range []string{"", "not json at all", `{"agent_dir":123}`} {
		out, err := runErase(t, script, dir, payload)
		if err != nil {
			t.Fatalf("payload %q must degrade without failing the turn: %v\n%s", payload, err, out)
		}
		body, err := os.ReadFile(filepath.Join(dir, "session.jsonl"))
		if err != nil || !strings.Contains(string(body), "one") {
			t.Fatalf("payload %q must leave the session intact: %q %v", payload, body, err)
		}
	}
}
