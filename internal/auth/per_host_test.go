package auth

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestCredentialsPathFor(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for url, want := range map[string]string{
		"":                                    "credentials.json",
		"https://studio.taufinity.io":         "credentials.json",
		"https://studio.taufinity.io:443/":    "credentials.json",
		"https://STUDIO.taufinity.io":         "credentials.json",
		"https://staging-studio.taufinity.io": "credentials-staging-studio.taufinity.io.json",
		"http://localhost:8090":               "credentials-localhost_8090.json",
		"http://127.0.0.1:54321":              "credentials-127.0.0.1_54321.json",
	} {
		if got := filepath.Base(CredentialsPathFor(url)); got != want {
			t.Errorf("CredentialsPathFor(%q) = %q, want %q", url, got, want)
		}
	}
}

// The bug this fixes: one credentials file for every host, so a staging
// login replaced the production session, and a staging process deleting
// "its" rejected session deleted production's.
func TestStagingCredentialsNeverTouchProduction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(func() { UseAPIURL("") })

	UseAPIURL("https://studio.taufinity.io")
	prod := &Credentials{AccessToken: "prod-a", RefreshToken: "prod-r", ExpiresAt: time.Now().Add(time.Hour)}
	if err := prod.Save(); err != nil {
		t.Fatal(err)
	}

	UseAPIURL("https://staging-studio.taufinity.io")
	if HasCredentials() {
		t.Fatal("staging sees production's credentials")
	}
	stg := &Credentials{AccessToken: "stg-a", RefreshToken: "stg-r", ExpiresAt: time.Now().Add(time.Hour)}
	if err := stg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := DeleteCredentials(); err != nil {
		t.Fatal(err)
	}

	got, err := LoadCredentialsFor("https://studio.taufinity.io")
	if err != nil {
		t.Fatalf("production credentials gone after a staging save+delete: %v", err)
	}
	if got.AccessToken != "prod-a" || got.RefreshToken != "prod-r" {
		t.Fatal("production credentials were overwritten by staging")
	}
}

func TestLoadedCredentialsSaveBackToTheirOwnFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(func() { UseAPIURL("") })
	c := &Credentials{AccessToken: "a", ExpiresAt: time.Now().Add(time.Hour)}
	c.BindTo("https://staging-studio.taufinity.io")
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	UseAPIURL("") // the process host changes; the loaded creds must not follow it
	loaded, err := LoadCredentialsFor("https://staging-studio.taufinity.io")
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.UpdateTokens("b", "r2", time.Now().Add(time.Hour), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(CredentialsPathFor("")); err == nil {
		t.Fatal("renewing staging credentials wrote the production file")
	}
}

func TestLockRefreshIsExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")

	unlock, err := LockRefresh(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	var got atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		u2, err := LockRefresh(context.Background(), path)
		if err != nil {
			t.Error(err)
			return
		}
		got.Store(true)
		u2()
	}()
	time.Sleep(200 * time.Millisecond)
	if got.Load() {
		t.Fatal("a second holder got the lock while the first still held it")
	}
	unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter never got the lock after release")
	}
	if !got.Load() {
		t.Fatal("waiter did not acquire")
	}
}

func TestLockRefreshHonoursContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.json")
	unlock, err := LockRefresh(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := LockRefresh(ctx, path); err == nil {
		t.Fatal("expected the context deadline while the lock is held")
	}
}
