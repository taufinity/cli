package commands

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// notabSetup: a watched folder, a session on a fake Studio and a page server
// with no tab connected.
func notabSetup(t *testing.T) (dir string, f *fakeBanner, srv *bannerServer, base string) {
	t.Helper()
	dir = templateDir(t, nil)
	f = newFakeBanner(t)
	srv, base, _ = startBanner(t, simpleReport(), map[string]string{"rows/0/index.html": "x"})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sess := &bannerSession{Fill: f.source(t, dir).Fill, Srv: srv}
	sess.Attach(ctx)
	startWatch(t, dir, "", func() { _, _ = sess.Changed(ctx) })
	return
}

func connectTab(t *testing.T, srv *bannerServer, base string) (disconnect func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for srv.Clients() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("tab never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return func() { cancel(); resp.Body.Close() }
}

func TestWatcherDoesNotRefillWithoutAnOpenTab(t *testing.T) {
	dir, f, _, _ := notabSetup(t)
	for i := 0; i < 3; i++ {
		_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte{byte('a' + i)}, 0o644)
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if got := f.callCount(); got != 0 {
		t.Fatalf("Studio got %d calls with no tab open", got)
	}
}

func TestWatcherRefillsOnceWhenATabConnects(t *testing.T) {
	dir, f, srv, base := notabSetup(t)
	for i := 0; i < 3; i++ {
		_ = os.WriteFile(filepath.Join(dir, "index.html"), []byte{byte('a' + i)}, 0o644)
		time.Sleep(200 * time.Millisecond)
	}
	if got := f.callCount(); got != 0 {
		t.Fatalf("calls before a tab: %d", got)
	}
	disconnect := connectTab(t, srv, base)
	deadline := time.Now().Add(5 * time.Second)
	for f.callCount() < 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	// a second tab and a reconnect must not refill again: nothing is stale
	disconnect()
	disconnect = connectTab(t, srv, base)
	defer disconnect()
	time.Sleep(500 * time.Millisecond)
	if got := f.callCount(); got != 1 {
		t.Fatalf("Studio got %d calls, want exactly 1", got)
	}
}
