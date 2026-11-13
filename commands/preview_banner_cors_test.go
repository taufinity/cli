package commands

import (
	"net/http"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
	"golang.org/x/image/font/gofont/goregular"
)

// The banner is framed with sandbox="allow-scripts", so its origin is
// opaque. Its own webfont and its own fetch of a JSON file are CORS requests
// to the banner port; both must load, or the preview shows a fallback font
// that a render would not.
func TestBannerLoadsOwnFontAndJSON(t *testing.T) {
	html := `<html><head><style>@font-face{font-family:X;src:url(f.ttf)}</style></head><body style="font-family:X">hi<script>
window.addEventListener("load", function () {
  var out = {};
  document.fonts.load("16px X").then(function (f) { out.font = f.length; }).catch(function (e) { out.font = "err " + e; })
  .then(function () {
    return fetch("data.json").then(function (r) { return r.text(); }).then(function (t) { out.fetch = t; }).catch(function (e) { out.fetch = "err " + e; });
  })
  .then(function () { setInterval(function () { parent.postMessage({type: "cors-probe", out: out}, "*"); }, 100); });
});
</script></body></html>`
	_, base, _ := startBanner(t, simpleReport(), map[string]string{
		"rows/0/index.html": html,
		"rows/0/f.ttf":      string(goregular.TTF),
		"rows/0/data.json":  `{"ok":1}`,
	})
	ctx := browserFor(t)
	if err := chromedp.Run(ctx, chromedp.Navigate(base+"/")); err != nil {
		t.Fatal(err)
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.addEventListener("message", function (e) {
  if (e.data && e.data.type === "cors-probe") window.__probe = JSON.stringify(e.data.out);
}); true`, nil)); err != nil {
		t.Fatal(err)
	}
	waitJS(t, ctx, `!!window.__probe`)
	var got string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`window.__probe`, &got)); err != nil {
		t.Fatal(err)
	}
	if want := `{"font":1,"fetch":"{\"ok\":1}"}`; got != want {
		t.Fatalf("banner font/fetch = %s, want %s", got, want)
	}
}

// The filled rows carry the customer's data. They are only served under the
// run's random key, so another website cannot read them, CORS or not; under
// the key, the banner's opaque origin gets the CORS header it needs.
func TestBannerFilesOnlyUnderTheRunKey(t *testing.T) {
	s, _, _ := startBanner(t, simpleReport(), map[string]string{
		"rows/0/index.html": "<html></html>",
		"rows/0/data.json":  `{"rows":"customer data"}`,
	})
	get := func(t *testing.T, u, origin string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	for name, u := range map[string]string{
		"no key":    s.BannerOrigin() + "/rows/0/data.json",
		"wrong key": s.BannerOrigin() + "/" + strings.Repeat("0", 32) + "/rows/0/data.json",
		"key only":  s.BannerBase() + "/",
	} {
		t.Run(name, func(t *testing.T) {
			for _, origin := range []string{"", "null", "https://evil.example"} {
				if resp := get(t, u, origin); resp.StatusCode != http.StatusNotFound || resp.Header.Get("Access-Control-Allow-Origin") != "" {
					t.Fatalf("origin %q: status %d, ACAO %q; want 404 without CORS", origin, resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
				}
			}
		})
	}
	t.Run("under the key", func(t *testing.T) {
		file := s.BannerBase() + "/rows/0/data.json"
		for origin, want := range map[string]string{"null": "null", "": "", "https://evil.example": "", "http://" + s.addr: ""} {
			resp := get(t, file, origin)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("origin %q: status %d", origin, resp.StatusCode)
			}
			if got := resp.Header.Get("Access-Control-Allow-Origin"); got != want {
				t.Fatalf("origin %q: ACAO %q, want %q", origin, got, want)
			}
			if !strings.Contains(strings.Join(resp.Header.Values("Vary"), ","), "Origin") {
				t.Fatalf("origin %q: no Vary: Origin", origin)
			}
		}
	})
	t.Run("key is random per run", func(t *testing.T) {
		other := newBannerServer()
		if len(s.bannerKey) != 32 || other.bannerKey == s.bannerKey || other.bannerKey == other.token {
			t.Fatalf("key %q / %q: want 32 hex, different per run, not the render token", s.bannerKey, other.bannerKey)
		}
	})
}
