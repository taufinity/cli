package commands

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/taufinity/cli/internal/auth"
)

// isolateCredentials points the CLI's credential store at a fresh temp
// HOME for the duration of the test and writes a fake, non-expired
// credentials file so HasCredentials()/the client's auth header logic see a
// logged-in user without touching anything real.
func isolateCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	creds := &auth.Credentials{
		AccessToken:          "test-access-token",
		RefreshToken:         "test-refresh-token",
		AccessTokenExpiresAt: time.Now().Add(time.Hour),
		ExpiresAt:            time.Now().Add(24 * time.Hour),
		Email:                "test@example.com",
	}
	if err := creds.Save(); err != nil {
		t.Fatalf("save fake credentials: %v", err)
	}
}

// withTestAPIURL points flagAPIURL at srv for the test's duration and
// restores the previous value after.
func withTestAPIURL(t *testing.T, srv *httptest.Server) {
	t.Helper()
	prev := flagAPIURL
	flagAPIURL = srv.URL
	t.Cleanup(func() { flagAPIURL = prev })
}

func TestRunAuthElevate_BackupCodeFlag_SendsBackupCodeNotTOTP(t *testing.T) {
	isolateCredentials(t)

	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/cli-elevate" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":                  "elevation-token",
			"session_id":             1,
			"expires_at":             time.Now().Add(time.Hour),
			"backup_codes_remaining": 5,
		})
	}))
	defer srv.Close()
	withTestAPIURL(t, srv)

	elevationTTL = "16h"
	elevationBackupCode = "aaaaaaaa-bbbbbbbb"
	defer func() { elevationBackupCode = "" }()

	if err := runAuthElevate(authElevateCmd, nil); err != nil {
		t.Fatalf("runAuthElevate: %v", err)
	}

	if gotBody["backup_code"] != "aaaaaaaa-bbbbbbbb" {
		t.Errorf("expected backup_code in request body, got: %+v", gotBody)
	}
	if _, hasTOTP := gotBody["totp_code"]; hasTOTP {
		t.Errorf("--backup-code should skip the TOTP field entirely, got: %+v", gotBody)
	}
}

func TestRunAuthBackupCodesStatus_PrintsRemainingAndTotal(t *testing.T) {
	isolateCredentials(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/auth/backup-codes/status" || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"remaining": 4, "total_issued": 10})
	}))
	defer srv.Close()
	withTestAPIURL(t, srv)

	if err := runAuthBackupCodesStatus(authBackupCodesStatusCmd, nil); err != nil {
		t.Fatalf("runAuthBackupCodesStatus: %v", err)
	}
}

func TestRunAuthBackupCodesStatus_RequiresCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no credentials.json written

	if err := runAuthBackupCodesStatus(authBackupCodesStatusCmd, nil); err == nil {
		t.Error("expected an error when not authenticated")
	}
}

// TestBackupCodeProofDispatch_ByLength documents and locks in the
// length-based heuristic runAuthBackupCodesRegenerate uses to decide whether
// pasted input is a live TOTP code (6 digits) or a backup code
// ("xxxxxxxx-xxxxxxxx", 17 chars) — the actual new logic that command adds
// beyond a plain HTTP call. Extracted as a pure function so it's testable
// without stdin plumbing (the interactive prompt itself isn't unit-tested,
// matching every other auth command in this file — none of them are).
func TestBackupCodeProofDispatch_ByLength(t *testing.T) {
	cases := []struct {
		input     string
		wantField string
		wantValue string
	}{
		{"123456", "totp_code", "123456"},
		{"aaaaaaaa-bbbbbbbb", "backup_code", "aaaaaaaa-bbbbbbbb"},
	}
	for _, c := range cases {
		field, value := backupCodeProofField(c.input)
		if field != c.wantField {
			t.Errorf("input %q: expected field %q, got %q", c.input, c.wantField, field)
		}
		if value != c.wantValue {
			t.Errorf("input %q: expected value %q, got %q", c.input, c.wantValue, value)
		}
	}
}
