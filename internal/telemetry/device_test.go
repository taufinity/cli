package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDeviceID_CreatesOnFirstRun(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	id, fr, err := loadDeviceID()
	if err != nil {
		t.Fatalf("loadDeviceID: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty device ID")
	}
	if !fr {
		t.Fatal("expected firstRun=true on initial call")
	}

	path := filepath.Join(tmp, ".config", "taufinity", "device.json")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Fatalf("device.json not created at %s", path)
	}
}

func TestLoadDeviceID_Idempotent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	id1, _, _ := loadDeviceID()
	id2, fr2, err := loadDeviceID()
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("device ID changed: %s → %s", id1, id2)
	}
	if fr2 {
		t.Fatal("expected firstRun=false on second call")
	}
}

func TestLoadDeviceID_ReadsExisting(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	dir := filepath.Join(tmp, ".config", "taufinity")
	os.MkdirAll(dir, 0700)
	want := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	data, _ := json.Marshal(deviceFile{DeviceID: want})
	os.WriteFile(filepath.Join(dir, "device.json"), data, 0600)

	got, fr, err := loadDeviceID()
	if err != nil {
		t.Fatalf("loadDeviceID: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if fr {
		t.Fatal("expected firstRun=false when file exists")
	}
}
