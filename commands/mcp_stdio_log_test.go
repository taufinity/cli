package commands

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// openMCPLogFileAt is a test helper that opens the session log at a custom
// path, mirroring openMCPLogFile but without touching the real config dir.
func openMCPLogFileAt(path string) (*os.File, error) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > mcpLogMaxBytes {
		_ = os.Truncate(path, 0)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	_, _ = f.WriteString("\n--- taufinity mcp stdio session started " + time.Now().UTC().Format(time.RFC3339) + " ---\n")
	return f, nil
}

func TestOpenMCPLogFile_CreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "studio.log")

	f, err := openMCPLogFileAt(path)
	if err != nil {
		t.Fatalf("openMCPLogFileAt: %v", err)
	}
	defer f.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("log file not created: %v", err)
	}
}

func TestOpenMCPLogFile_WritesSessionHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "studio.log")

	f, err := openMCPLogFileAt(path)
	if err != nil {
		t.Fatalf("openMCPLogFileAt: %v", err)
	}
	f.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "taufinity mcp stdio session started") {
		t.Errorf("session header not found in log; got: %q", content)
	}
}

func TestOpenMCPLogFile_AppendsAcrossSessions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "studio.log")

	for i := 0; i < 3; i++ {
		f, err := openMCPLogFileAt(path)
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		f.Close()
	}

	data, _ := os.ReadFile(path)
	count := strings.Count(string(data), "session started")
	if count != 3 {
		t.Errorf("expected 3 session headers, got %d", count)
	}
}

func TestOpenMCPLogFile_RotatesWhenOverLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "studio.log")

	// Write a file larger than the rotation limit.
	big := make([]byte, mcpLogMaxBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if err := os.WriteFile(path, big, 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	f, err := openMCPLogFileAt(path)
	if err != nil {
		t.Fatalf("openMCPLogFileAt: %v", err)
	}
	f.Close()

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() >= int64(mcpLogMaxBytes) {
		t.Errorf("expected file smaller than %d after rotation, got %d", mcpLogMaxBytes, fi.Size())
	}
}

func TestMCPBridge_StderrTeesToLogFile(t *testing.T) {
	upstream := newMockUpstream()
	defer upstream.close()

	// Wire a real file as the "log file" alongside a buffer as stderr.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "studio.log")
	logFile, err := openMCPLogFileAt(logPath)
	if err != nil {
		t.Fatalf("openMCPLogFileAt: %v", err)
	}
	defer logFile.Close()

	pr, pw := io.Pipe()
	outBuf := &threadSafeBuffer{}
	logAndStderr := io.MultiWriter(&threadSafeBuffer{}, logFile)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = RunStdioBridge(ctx, StdioBridgeConfig{
			UpstreamURL: upstream.url(),
			Token:       "test-token",
			UserAgent:   "taufinity-cli/test",
			Timeout:     3 * time.Second,
			Stdin:       pr,
			Stdout:      outBuf,
			Stderr:      logAndStderr,
		})
	}()

	// Send a JSON-RPC ping so the bridge does something.
	frame, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "ping",
	})
	_, _ = pw.Write(append(frame, '\n'))

	// Give bridge time to process and write.
	time.Sleep(300 * time.Millisecond)
	pw.Close()
	<-done

	logFile.Close()

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) == 0 {
		t.Error("log file is empty; expected at least a session header")
	}
	if !strings.Contains(string(data), "session started") {
		t.Errorf("session header missing from log; content: %q", string(data))
	}
}
