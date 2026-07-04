package terms

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestShowOnce(t *testing.T) {
	// Point flag file into a temp dir so tests are isolated.
	tmp := t.TempDir()
	flag := filepath.Join(tmp, ".config", "taufinity", "privacy_accepted")

	origHome := os.Getenv("HOME")
	t.Setenv("HOME", tmp)
	t.Cleanup(func() { os.Setenv("HOME", origHome) })

	var buf bytes.Buffer

	// First call: notice should appear.
	ShowOnce(&buf)
	if buf.Len() == 0 {
		t.Fatal("expected notice on first call, got nothing")
	}
	if _, err := os.Stat(flag); err != nil {
		t.Fatalf("flag file not created after first call: %v", err)
	}

	// Second call: no-op.
	buf.Reset()
	ShowOnce(&buf)
	if buf.Len() != 0 {
		t.Fatalf("expected no output on second call, got: %q", buf.String())
	}
}

func TestShowOnceOptOut(t *testing.T) {
	t.Setenv(EnvNoTelemetry, "1")
	t.Setenv("HOME", t.TempDir())

	var buf bytes.Buffer
	ShowOnce(&buf)
	if buf.Len() != 0 {
		t.Fatalf("expected no output when opt-out set, got: %q", buf.String())
	}
}
