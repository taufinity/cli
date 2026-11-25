package commands

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A save while Studio asked us to wait fills nothing, so the preview on screen
// is older than the file on disk. It has to stay stale, or the next tab to
// connect trusts a fill that never happened and shows the old banner forever.
func TestChangedKeepsThePreviewStaleWhenRefreshIsPaused(t *testing.T) {
	dir := templateDir(t, nil)
	f := newFakeBanner(t)
	srv, base, _ := startBanner(t, simpleReport(), map[string]string{"rows/0/index.html": "x"})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sess := &bannerSession{Fill: f.source(t, dir).Fill, Srv: srv}

	disconnect := connectTab(t, srv, base)
	defer disconnect()

	sess.pauseUntil = time.Now().Add(time.Minute)
	filled, err := sess.Changed(ctx)
	if !errors.Is(err, errBannerPaused) {
		t.Fatalf("want errBannerPaused, got filled=%v err=%v", filled, err)
	}
	if got := f.callCount(); got != 0 {
		t.Fatalf("Studio was called %d times while refreshing was paused", got)
	}
	if was := sess.setStale(false); !was {
		t.Fatal("the preview was marked fresh although nothing was filled")
	}
}
