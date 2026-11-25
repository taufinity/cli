package commands

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// One headless Chrome serves every browser test in the package; each test
// gets its own tab. Starting Chrome per test made the suite start it about
// fifteen times, and on a loaded CI runner one of those starts took 54 s and
// timed out (2026-11-16, run 43879693963). Every test serves its page on a port
// of its own, so tabs never share an origin.
var shared struct {
	once    sync.Once
	browser context.Context
	out     *lockedBuffer
	err     error
	stop    func()
}

func startSharedBrowser() {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
	)
	opts = append(opts, chromedp.NoSandbox) // CI runners (root, containers) need it
	// The first Chrome start on a fresh CI runner can take longer than
	// chromedp's default 20 s to print its DevTools address (seen on the
	// ubuntu-26.04 image of 2026-11-11, build 196).
	opts = append(opts, chromedp.WSURLReadTimeout(90*time.Second))
	shared.out = &lockedBuffer{}
	opts = append(opts, chromedp.CombinedOutput(shared.out))
	actx, cancelA := chromedp.NewExecAllocator(context.Background(), opts...)
	// The CI failure of 2026-11-16 was the connect to Chrome's DevTools
	// address, which chromedp gives 10 s by default, not the address itself.
	bctx, cancelB := chromedp.NewContext(actx, chromedp.WithBrowserOption(chromedp.WithDialTimeout(60*time.Second)))
	shared.browser = bctx
	shared.stop = func() { cancelB(); cancelA() }
	shared.err = chromedp.Run(bctx) // starts Chrome and its first tab
}

// stopSharedBrowser is called from TestMain once every test has run.
func stopSharedBrowser() {
	if shared.stop != nil {
		shared.stop()
	}
}

// browserFor gives the test a fresh tab in the shared headless Chrome. Chrome
// is required: a machine without it fails the test, it does not skip it.
func browserFor(t *testing.T) context.Context {
	t.Helper()
	shared.once.Do(startSharedBrowser)
	if shared.err != nil {
		t.Fatalf("Chrome kon niet starten (is Chrome geinstalleerd?): %v\nuitvoer van Chrome:\n%s", shared.err, shared.out.tail(4096))
	}
	// A tab, not a browser context of its own: headless Chrome refuses a new
	// tab in a fresh browser context (-32000, no browser is open). Tests do not
	// share an origin, since every test server has its own port.
	tab, cancelTab := chromedp.NewContext(shared.browser)
	ctx, cancelT := context.WithTimeout(tab, 90*time.Second)
	t.Cleanup(func() { cancelT(); cancelTab() })
	if err := chromedp.Run(ctx); err != nil {
		t.Fatalf("nieuw tabblad in Chrome: %v\nuitvoer van Chrome:\n%s", err, shared.out.tail(4096))
	}
	return ctx
}

func jsBool(t *testing.T, ctx context.Context, expr string) bool {
	t.Helper()
	var v bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &v)); err != nil {
		t.Fatal(err)
	}
	return v
}

func waitJS(t *testing.T, ctx context.Context, expr string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Poll(expr, nil, chromedp.WithPollingTimeout(15*time.Second))); err != nil {
		t.Fatalf("wachten op %q: %v", expr, err)
	}
}

const (
	jsModalOpen = `!document.getElementById("render-modal").hidden`
)

func openPage(t *testing.T, ctx context.Context, url string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Navigate(url)); err != nil {
		t.Fatal(err)
	}
	waitJS(t, ctx, `document.querySelectorAll("#row-picker option").length > 0`)
	// Wait for the banner to report as well. Its report shows the scrubber or
	// the "geen tijdlijn" note above the render controls, which moves them down
	// by about 32 px; a radio is 13 px high. A click whose coordinates were
	// taken before that report and dispatched after it lands above the control,
	// which is how gif_choice_sends_gif failed on CI with the PNG price.
	waitJS(t, ctx, jsBannerReported)
}

// jsBannerReported is true once nothing above the render controls will still
// appear: the banner sent its driver (scrubber or note), or the row is an error.
const jsBannerReported = `!document.getElementById("scrubber").hidden ||
	!document.getElementById("scrubber-note").hidden ||
	!document.getElementById("error-overlay").hidden`

func (f *fakeRenderStudio) counts() (uploads, renders int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploads, len(f.renders)
}

func TestPreviewRenderBrowser(t *testing.T) {
	setup := func(t *testing.T) (*fakeRenderStudio, *bannerServer, context.Context) {
		fs := newFakeRenderStudio(t)
		srv, base := startRenderServer(t, fs)
		ctx := browserFor(t)
		openPage(t, ctx, base+"/")
		return fs, srv, ctx
	}
	click := func(t *testing.T, ctx context.Context, sel string) {
		t.Helper()
		if err := chromedp.Run(ctx, chromedp.Click(sel, chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("first_click_shows_modal", func(t *testing.T) {
		fs, _, ctx := setup(t)
		click(t, ctx, "#render-button")
		waitJS(t, ctx, jsModalOpen)
		var text string
		if err := chromedp.Run(ctx, chromedp.Text("#render-modal", &text, chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"echte render", "normale prijs", "normale tarief voor dit type banner", "Bevestigen", "Annuleren"} {
			if !strings.Contains(text, want) {
				t.Errorf("modal lacks %q: %q", want, text)
			}
		}
		// No figure: the agreement's price list is the one source, a number
		// here only drifts from it (the GIF price was once shown wrong in
		// a demo).
		if strings.Contains(text, "credit") {
			t.Errorf("modal names a price in credits: %q", text)
		}
		if u, r := fs.counts(); u != 0 || r != 0 {
			t.Fatalf("studio saw %d uploads %d renders before confirming", u, r)
		}
	})

	t.Run("cancel_sends_nothing", func(t *testing.T) {
		fs, srv, ctx := setup(t)
		click(t, ctx, "#render-button")
		waitJS(t, ctx, jsModalOpen)
		click(t, ctx, "#render-cancel")
		waitJS(t, ctx, `!`+jsModalOpen)
		time.Sleep(300 * time.Millisecond)
		if u, r := fs.counts(); u != 0 || r != 0 {
			t.Fatalf("studio saw %d uploads %d renders", u, r)
		}
		if srv.renderConfirmed() {
			t.Fatal("cancel stored a confirmation")
		}
	})

	t.Run("confirm_renders_and_remembers", func(t *testing.T) {
		fs, srv, ctx := setup(t)
		click(t, ctx, "#render-button")
		waitJS(t, ctx, jsModalOpen)
		click(t, ctx, "#render-confirm")
		waitJS(t, ctx, `(function(){var i=document.getElementById("render-image");return !!i && i.complete && i.naturalWidth===2})()`)
		if u, r := fs.counts(); u != 1 || r != 1 {
			t.Fatalf("studio saw %d uploads %d renders", u, r)
		}
		var text string
		if err := chromedp.Run(ctx, chromedp.Text("#render-result", &text, chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text, "Gelukt") || !strings.Contains(text, "12345") {
			t.Fatalf("result text = %q", text)
		}
		if !srv.renderConfirmed() {
			t.Fatal("confirmation not remembered")
		}
		if !jsBool(t, ctx, `!document.getElementById("render-button").disabled`) {
			t.Fatal("button stays disabled")
		}
	})

	t.Run("second_click_skips_modal", func(t *testing.T) {
		fs, srv, ctx := setup(t)
		base := strings.TrimSuffix(srv.URL(), "/")
		click(t, ctx, "#render-button")
		waitJS(t, ctx, jsModalOpen)
		click(t, ctx, "#render-confirm")
		waitJS(t, ctx, `!!document.getElementById("render-image")`)
		openPage(t, ctx, base+"/") // reload
		if !jsBool(t, ctx, `document.getElementById("render-image") === null`) {
			t.Fatal("result survived a reload unexpectedly")
		}
		click(t, ctx, "#render-button")
		waitJS(t, ctx, `!!document.getElementById("render-image")`)
		if jsBool(t, ctx, jsModalOpen) {
			t.Fatal("modal shown on second render")
		}
		if u, r := fs.counts(); u != 2 || r != 2 {
			t.Fatalf("studio saw %d uploads %d renders", u, r)
		}
	})

	t.Run("gif_choice_sends_gif", func(t *testing.T) {
		fs, _, ctx := setup(t)
		click(t, ctx, `input[name="render-format"][value="gif"]`)
		click(t, ctx, "#render-button")
		waitJS(t, ctx, jsModalOpen)
		var text string
		if err := chromedp.Run(ctx, chromedp.Text("#render-modal-price", &text, chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text, "normale tarief voor dit type banner") {
			t.Fatalf("price = %q", text)
		}
		click(t, ctx, "#render-confirm")
		waitJS(t, ctx, `document.getElementById("render-result").hidden === false`)
		fs.mu.Lock()
		defer fs.mu.Unlock()
		if len(fs.renders) != 1 || fs.renders[0]["format"] != "gif" || fs.renders[0]["frames"] != float64(10) {
			t.Fatalf("renders = %v", fs.renders)
		}
	})
}

// lockedBuffer collects Chrome's output, which chromedp writes from its own
// goroutine, for the failure message.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) tail(n int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.Bytes()
	if len(s) > n {
		s = s[len(s)-n:]
	}
	return string(s)
}
