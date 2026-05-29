package telemetry

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

type deviceFile struct {
	DeviceID string `json:"device_id"`
}

// loadDeviceID returns (id, firstRun, err).
// firstRun is true when device.json did not exist before this call.
func loadDeviceID() (string, bool, error) {
	path, err := deviceFilePath()
	if err != nil {
		return "", false, err
	}

	data, err := os.ReadFile(path)
	if err == nil {
		var f deviceFile
		if json.Unmarshal(data, &f) == nil && f.DeviceID != "" {
			return f.DeviceID, false, nil
		}
	}

	// First run: generate and persist a new UUID.
	id := uuid.New().String()
	f := deviceFile{DeviceID: id}
	raw, _ := json.MarshalIndent(f, "", "  ")
	if werr := os.WriteFile(path, raw, 0600); werr != nil {
		// Return the ID even if we can't save — one-run telemetry is better than none.
		return id, true, nil
	}
	return id, true, nil
}

func deviceFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "taufinity")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "device.json"), nil
}
