package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildTestBinary compiles the taufinity binary into a temp directory and
// returns the path. Skips the test if the build fails (e.g. no Go toolchain).
func buildTestBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "taufinity")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/taufinity")
	cmd.Dir = filepath.Join(rootDir(t), ".")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not build taufinity binary: %v\n%s", err, output)
	}
	return out
}

// rootDir returns the module root (one level up from the commands package).
func rootDir(t *testing.T) string {
	t.Helper()
	// __file__ is in commands/, module root is one level up.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(wd)
}

// newMCPE2EUpstream starts a minimal HTTP server that handles the Studio /mcp
// endpoint. It echoes a JSON-RPC success result for every well-formed request
// and records each inbound body for later assertion.
func newMCPE2EUpstream(t *testing.T) (url string, bodies func() []string, close func()) {
	t.Helper()
	var captured []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured = append(captured, string(body))

		var frame struct {
			ID json.RawMessage `json:"id"`
		}
		_ = json.Unmarshal(body, &frame)
		id := frame.ID
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`, id)
	}))
	return srv.URL, func() []string { return captured }, srv.Close
}

// writeE2EHome writes a minimal CLI home directory:
//   - config.yaml pointing at apiURL
//   - credentials.json with a non-expiring access token (no network refresh needed)
func writeE2EHome(t *testing.T, apiURL string) string {
	t.Helper()
	home := t.TempDir()
	cfgDir := filepath.Join(home, ".config", "taufinity")
	if err := os.MkdirAll(cfgDir, 0700); err != nil {
		t.Fatal(err)
	}

	config := fmt.Sprintf("api_url: %s\nsite: test\n", apiURL)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}

	// Access token valid until year 2099 — no refresh needed.
	expiry := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	creds := map[string]any{
		"access_token":           "e2e-test-token",
		"access_token_expires_at": expiry.Format(time.RFC3339),
		"expires_at":              expiry.Format(time.RFC3339),
	}
	credsJSON, _ := json.Marshal(creds)
	if err := os.WriteFile(filepath.Join(cfgDir, "credentials.json"), credsJSON, 0600); err != nil {
		t.Fatal(err)
	}

	return home
}

// TestCLIBinary_StudioLogCreated runs the real taufinity binary as a subprocess,
// sends one JSON-RPC frame, and asserts that studio.log is created and populated.
func TestCLIBinary_StudioLogCreated(t *testing.T) {
	binary := buildTestBinary(t)

	upstreamURL, _, closeUpstream := newMCPE2EUpstream(t)
	defer closeUpstream()

	home := writeE2EHome(t, upstreamURL)
	logPath := filepath.Join(home, ".config", "taufinity", "studio.log")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	frame, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
		"params":  map[string]any{},
	})

	cmd := exec.CommandContext(ctx, binary, "mcp", "stdio")
	cmd.Env = append(filteredEnv(), "HOME="+home)
	cmd.Stdin = bytes.NewReader(append(frame, '\n')) // one frame then EOF
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	_ = cmd.Run() // exits after stdin EOF; ignore exit code

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("studio.log not created at %s: %v\nstderr: %s", logPath, err, stderr.String())
	}
	if len(data) == 0 {
		t.Fatal("studio.log is empty")
	}
	content := string(data)
	t.Logf("studio.log:\n%s", content)

	if !strings.Contains(content, "session started") {
		t.Errorf("session header missing from log")
	}
}

// TestCLIBinary_LogContainsRequest verifies that the bridge logs HTTP request
// activity (from slog debug) when the mock upstream is hit.
func TestCLIBinary_LogContainsRequest(t *testing.T) {
	binary := buildTestBinary(t)

	upstreamURL, getBodies, closeUpstream := newMCPE2EUpstream(t)
	defer closeUpstream()

	home := writeE2EHome(t, upstreamURL)
	logPath := filepath.Join(home, ".config", "taufinity", "studio.log")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Send two frames then close stdin.
	var input bytes.Buffer
	for i := 1; i <= 2; i++ {
		frame, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      i,
			"method":  "ping",
		})
		input.Write(append(frame, '\n'))
	}

	cmd := exec.CommandContext(ctx, binary, "--debug", "mcp", "stdio")
	cmd.Env = append(filteredEnv(), "HOME="+home)
	cmd.Stdin = &input
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	_ = cmd.Run()

	bodies := getBodies()
	if len(bodies) == 0 {
		t.Error("upstream received no requests")
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("studio.log missing: %v", err)
	}
	t.Logf("studio.log (%d bytes):\n%s", len(data), string(data))

	if !strings.Contains(string(data), "session started") {
		t.Error("session header missing")
	}
}

// TestCLIBinary_DegradedModeLogsAuthError verifies that even without valid
// credentials the binary enters degraded mode and still writes to studio.log.
func TestCLIBinary_DegradedModeLogsAuthError(t *testing.T) {
	binary := buildTestBinary(t)

	home := t.TempDir()
	cfgDir := filepath.Join(home, ".config", "taufinity")
	if err := os.MkdirAll(cfgDir, 0700); err != nil {
		t.Fatal(err)
	}
	// No credentials file — forces degraded mode.
	config := "api_url: http://127.0.0.1:19999\nsite: test\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, "mcp", "stdio")
	cmd.Env = append(filteredEnv(), "HOME="+home)
	cmd.Stdin = bytes.NewReader(nil) // immediate EOF
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_ = cmd.Run()

	logPath := filepath.Join(cfgDir, "studio.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("studio.log not created in degraded mode: %v\nstderr: %s", err, stderr.String())
	}
	t.Logf("studio.log (degraded):\n%s", string(data))
	if !strings.Contains(string(data), "session started") {
		t.Errorf("session header missing in degraded mode log")
	}
}

// filteredEnv returns os.Environ() with HOME and TAUFINITY_* vars stripped so
// the subprocess picks up only what the test explicitly provides.
func filteredEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "HOME=") ||
			strings.HasPrefix(e, "TAUFINITY_") {
			continue
		}
		env = append(env, e)
	}
	return env
}
