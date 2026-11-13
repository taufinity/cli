package commands

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
)

// browserFor starts headless Chrome. Chrome is required: a machine without it
// fails the test, it does not skip it.
func browserFor(t *testing.T) context.Context {
	t.Helper()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
	)
	opts = append(opts, chromedp.NoSandbox) // CI runners (root, containers) need it
	actx, cancelA := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancelC := chromedp.NewContext(actx)
	ctx, cancelT := context.WithTimeout(ctx, 60*time.Second)
	t.Cleanup(func() { cancelT(); cancelC(); cancelA() })
	if err := chromedp.Run(ctx); err != nil {
		t.Fatalf("Chrome kon niet starten (is Chrome geinstalleerd?): %v", err)
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
	jsConfirmed = `localStorage.getItem("taufinity.preview.renderConfirmed")`
)

func openPage(t *testing.T, ctx context.Context, url string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Navigate(url)); err != nil {
		t.Fatal(err)
	}
	waitJS(t, ctx, `document.querySelectorAll("#row-picker option").length > 0`)
}

func (f *fakeRenderStudio) counts() (uploads, renders int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.uploads, len(f.renders)
}

func TestPreviewRenderBrowser(t *testing.T) {
	setup := func(t *testing.T) (*fakeRenderStudio, string, context.Context) {
		fs := newFakeRenderStudio(t)
		_, base := startRenderServer(t, fs)
		ctx := browserFor(t)
		openPage(t, ctx, base+"/")
		return fs, base, ctx
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
		for _, want := range []string{"echte render", "normale prijs", "1 credit per stilstaand beeld", "Bevestigen", "Annuleren"} {
			if !strings.Contains(text, want) {
				t.Errorf("modal lacks %q: %q", want, text)
			}
		}
		if u, r := fs.counts(); u != 0 || r != 0 {
			t.Fatalf("studio saw %d uploads %d renders before confirming", u, r)
		}
	})

	t.Run("cancel_sends_nothing", func(t *testing.T) {
		fs, _, ctx := setup(t)
		click(t, ctx, "#render-button")
		waitJS(t, ctx, jsModalOpen)
		click(t, ctx, "#render-cancel")
		waitJS(t, ctx, `!`+jsModalOpen)
		time.Sleep(300 * time.Millisecond)
		if u, r := fs.counts(); u != 0 || r != 0 {
			t.Fatalf("studio saw %d uploads %d renders", u, r)
		}
		if !jsBool(t, ctx, jsConfirmed+` === null`) {
			t.Fatal("cancel stored a confirmation")
		}
	})

	t.Run("confirm_renders_and_remembers", func(t *testing.T) {
		fs, _, ctx := setup(t)
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
		if !jsBool(t, ctx, jsConfirmed+` === "1"`) {
			t.Fatal("confirmation not remembered")
		}
		if !jsBool(t, ctx, `!document.getElementById("render-button").disabled`) {
			t.Fatal("button stays disabled")
		}
	})

	t.Run("second_click_skips_modal", func(t *testing.T) {
		fs, base, ctx := setup(t)
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
		if !strings.Contains(text, "2 credits voor een animatie van 10 frames") {
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
