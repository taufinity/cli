package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/taufinity/cli/internal/config"
)

// ElevationToken holds a short-lived CLI elevation credential.
type ElevationToken struct {
	Token     string    `json:"token"`
	SessionID uint      `json:"session_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// elevationTokenPath returns the path to the stored elevation token file.
func elevationTokenPath() string {
	return filepath.Join(config.Dir(), "elevation.token")
}

// SaveElevationToken persists an elevation token to disk with secure permissions.
func SaveElevationToken(token string, sessionID uint, expiresAt time.Time) error {
	dir := config.Dir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	et := ElevationToken{Token: token, SessionID: sessionID, ExpiresAt: expiresAt}
	data, err := json.Marshal(et)
	if err != nil {
		return fmt.Errorf("marshal elevation token: %w", err)
	}
	return os.WriteFile(elevationTokenPath(), data, 0600)
}

// LoadElevationToken reads the stored elevation token from disk.
// Returns ("", 0, zero, nil) when no token file exists.
func LoadElevationToken() (token string, sessionID uint, expiresAt time.Time, err error) {
	data, err := os.ReadFile(elevationTokenPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", 0, time.Time{}, nil
		}
		return "", 0, time.Time{}, fmt.Errorf("read elevation token: %w", err)
	}
	var et ElevationToken
	if err := json.Unmarshal(data, &et); err != nil {
		return "", 0, time.Time{}, fmt.Errorf("corrupt elevation token: %w", err)
	}
	return et.Token, et.SessionID, et.ExpiresAt, nil
}

// RemoveElevationToken deletes the stored elevation token file.
func RemoveElevationToken() error {
	err := os.Remove(elevationTokenPath())
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
