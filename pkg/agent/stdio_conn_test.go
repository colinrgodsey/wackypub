package agent

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestStdioConn_ReadWrite(t *testing.T) {
	// rIn -> wIn (for stdin: we write to wIn)
	rIn, wIn, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer rIn.Close()

	// rOut -> wOut (for stdout: we read from rOut)
	rOut, wOut, err := os.Pipe()
	if err != nil {
		wIn.Close()
		t.Fatalf("pipe: %v", err)
	}
	defer wOut.Close()

	conn := newStdioConn(nil, wIn, rOut)
	defer conn.Close()

	if conn.LocalAddr().Network() != "stdio" || conn.RemoteAddr().Network() != "stdio" {
		t.Errorf("expected stdio network, got local=%s remote=%s", conn.LocalAddr().Network(), conn.RemoteAddr().Network())
	}

	// Test Write
	go func() {
		_, _ = conn.Write([]byte("hello stdio"))
	}()

	buf := make([]byte, 64)
	n, err := rIn.Read(buf)
	if err != nil {
		t.Fatalf("failed reading from rIn: %v", err)
	}
	if string(buf[:n]) != "hello stdio" {
		t.Errorf("expected 'hello stdio', got %q", string(buf[:n]))
	}

	// Test Read
	go func() {
		_, _ = wOut.Write([]byte("response from bridge"))
	}()

	readBuf := make([]byte, 64)
	n, err = conn.Read(readBuf)
	if err != nil {
		t.Fatalf("failed reading from conn: %v", err)
	}
	if string(readBuf[:n]) != "response from bridge" {
		t.Errorf("expected 'response from bridge', got %q", string(readBuf[:n]))
	}
}

func TestStdioConn_DeadlinePropagation(t *testing.T) {
	rIn, wIn, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer rIn.Close()
	defer wIn.Close()

	rOut, wOut, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer rOut.Close()
	defer wOut.Close()

	conn := newStdioConn(nil, wIn, rOut)
	defer conn.Close()

	// 1. Set past read deadline
	past := time.Now().Add(-100 * time.Millisecond)
	if err := conn.SetReadDeadline(past); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	buf := make([]byte, 10)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatalf("expected deadline exceeded error on Read, got nil")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		if !os.IsTimeout(err) {
			t.Errorf("expected timeout error, got: %v", err)
		}
	}

	// 2. Clear deadline and verify read succeeds
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clearing deadline: %v", err)
	}

	go func() {
		time.Sleep(10 * time.Millisecond)
		_, _ = wOut.Write([]byte("ok"))
	}()

	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("expected successful read after clearing deadline, got: %v", err)
	}
	if string(buf[:n]) != "ok" {
		t.Errorf("expected 'ok', got %q", string(buf[:n]))
	}

	// 3. Set past write deadline
	if err := conn.SetWriteDeadline(past); err != nil {
		t.Fatalf("SetWriteDeadline: %v", err)
	}
	// Write with past deadline: on Linux pipes with empty buffer, Write might succeed if not filled,
	// or timeout. SetDeadline should return nil and propagate to both.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatalf("SetDeadline: %v", err)
	}
}

func TestStdioConn_CloseWaitsForCommand(t *testing.T) {
	cmd := exec.Command("sleep", "0.05")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	conn := newStdioConn(cmd, stdin, stdout)
	err = conn.Close()
	if err != nil {
		t.Errorf("Close returned error: %v", err)
	}

	// Second Close should be a clean idempotent no-op
	err2 := conn.Close()
	if err2 != nil {
		t.Errorf("second Close returned error: %v", err2)
	}

	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Errorf("expected command to be reaped and exited")
	}
}
