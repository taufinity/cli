package commands

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

type chromedpCtx = context.Context

func jsQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// threeRows is a report as the CLI hands it to the page: labels already the
// file lines, values per row.
func threeRows() *bannerReport {
	return &bannerReport{Entry: "index.html", Width: 300, Height: 250,
		Fields: []bannerName{"id", "titel", "beeld_url", "genre"},
		Rows: []bannerRow{
			{Index: 0, Label: "regel 2", Values: map[string]string{"id": "SC-1", "titel": "Koude Haven", "beeld_url": "img/a.svg", "genre": "Thriller"}},
			{Index: 1, Label: "regel 3", Values: map[string]string{"id": "SC-2", "titel": "Kaas & Liefde", "beeld_url": "img/b.svg", "genre": "Romcom"}},
			{Index: 2, Label: "regel 4", Values: map[string]string{"id": "SC-3", "titel": "De Zwarte Molen", "beeld_url": "img/c.svg", "genre": "Horror"}},
		}}
}

func jsString(t *testing.T, ctx chromedpCtx, expr string) string {
	t.Helper()
	var v string
	if err := chromedp.Run(ctx, chromedp.Evaluate(expr, &v)); err != nil {
		t.Fatal(err)
	}
	return v
}

const jsOptions = `Array.from(document.querySelectorAll("#row-picker option")).map(function (o) { return o.textContent; }).join("|")`

func TestPreviewPageRowsBrowser(t *testing.T) {
	setup := func(t *testing.T, hash string) chromedpCtx {
		files := map[string]string{}
		for _, i := range []string{"0", "1", "2"} {
			files["rows/"+i+"/index.html"] = "<html><body>" + i + "</body></html>"
		}
		_, base, _ := startBanner(t, threeRows(), files)
		ctx := browserFor(t)
		if !strings.Contains(hash, "view=all") {
			openPage(t, ctx, base+"/"+hash)
			return ctx
		}
		// The overview has no single banner to wait for (openPage does).
		if err := chromedp.Run(ctx, chromedp.Navigate(base+"/"+hash)); err != nil {
			t.Fatal(err)
		}
		waitJS(t, ctx, `document.querySelectorAll("#overview .cell").length === 3`)
		return ctx
	}
	typeSearch := func(t *testing.T, ctx chromedpCtx, q string) {
		t.Helper()
		if err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){var s=document.getElementById("row-search");s.value=`+jsQuote(q)+`;s.dispatchEvent(new Event("input"));})()`, nil)); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("options_name_line_and_title", func(t *testing.T) {
		ctx := setup(t, "")
		got := jsString(t, ctx, jsOptions)
		// The title, not the id, the link or the internal index.
		if got != "regel 2 · Koude Haven|regel 3 · Kaas & Liefde|regel 4 · De Zwarte Molen" {
			t.Fatalf("options = %q", got)
		}
	})

	t.Run("search_any_column", func(t *testing.T) {
		ctx := setup(t, "")
		typeSearch(t, ctx, "romcom") // a genre, not shown in the option by default
		if got := jsString(t, ctx, jsOptions); got != "regel 3 · Romcom" {
			t.Fatalf("options after search = %q", got)
		}
		if got := jsString(t, ctx, `document.getElementById("row-picker").value`); got != "1" {
			t.Fatalf("selected = %q, want the only match (index 1)", got)
		}
		if got := jsString(t, ctx, `document.getElementById("row-search-count").textContent`); got != "1 regel" {
			t.Fatalf("count = %q", got)
		}
		typeSearch(t, ctx, "sc-")
		if got := jsString(t, ctx, `document.getElementById("row-search-count").textContent`); got != "3 regels" {
			t.Fatalf("count for sc- = %q", got)
		}
		typeSearch(t, ctx, "")
		if !jsBool(t, ctx, `document.getElementById("row-search-count").hidden`) {
			t.Fatal("count stays visible without a search")
		}
	})

	t.Run("overview_back_to_first_row", func(t *testing.T) {
		// The bug: in "Alle regels" the picker showed regel 1, and choosing
		// regel 1 changed nothing, so the overview stayed.
		ctx := setup(t, "#row=0&view=all")
		waitJS(t, ctx, `!document.getElementById("overview").hidden`)
		if !jsBool(t, ctx, `document.getElementById("render-panel").hidden`) {
			t.Fatal("the render panel shows in the overview, where it is unclear which row it renders")
		}
		if got := jsString(t, ctx, `document.getElementById("row-picker").options[0].textContent`); got != "alle regels" {
			t.Fatalf("first option in the overview = %q", got)
		}
		if err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){var s=document.getElementById("row-picker");s.value="0";s.dispatchEvent(new Event("change"));})()`, nil)); err != nil {
			t.Fatal(err)
		}
		waitJS(t, ctx, `document.getElementById("overview").hidden && !document.getElementById("single").hidden`)
		if got := jsString(t, ctx, `document.getElementById("render-panel").hidden + " " + document.getElementById("render-title").textContent`); got != "false Echte render van regel 2" {
			t.Fatalf("render panel in the row view = %q", got)
		}
		if !strings.Contains(jsString(t, ctx, `location.hash`), "view=row") {
			t.Fatalf("hash = %q", jsString(t, ctx, `location.hash`))
		}
	})

	t.Run("overview_filtered_and_label_opens_row", func(t *testing.T) {
		ctx := setup(t, "#view=all")
		waitJS(t, ctx, `!document.getElementById("overview").hidden`)
		typeSearch(t, ctx, "molen")
		if got := jsString(t, ctx, `Array.from(document.querySelectorAll("#overview .cell .label")).map(function(l){return l.textContent;}).join("|")`); got != "regel 4" {
			t.Fatalf("overview after search = %q", got)
		}
		if err := chromedp.Run(ctx, chromedp.Click("#overview .cell .label", chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
		waitJS(t, ctx, `document.getElementById("overview").hidden`)
		if got := jsString(t, ctx, `document.getElementById("row-picker").value`); got != "2" {
			t.Fatalf("selected after clicking the label = %q", got)
		}
		typeSearch(t, ctx, "bestaat niet")
		if err := chromedp.Run(ctx, chromedp.Click("#overview-toggle", chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
		if got := jsString(t, ctx, `document.getElementById("overview").textContent`); !strings.Contains(got, "Geen regel bevat") {
			t.Fatalf("empty overview says %q", got)
		}
	})
}

func TestPreviewPageRenderStateBrowser(t *testing.T) {
	t.Run("no_timeline_no_gif", func(t *testing.T) {
		fs := newFakeRenderStudio(t)
		_, base := startRenderServerWith(t, fs, "<html><body>geen tijdlijn</body></html>")
		ctx := browserFor(t)
		openPage(t, ctx, base+"/")
		waitJS(t, ctx, `!document.getElementById("scrubber-note").hidden`)
		if !jsBool(t, ctx, `document.querySelector('input[name="render-format"][value="gif"]').disabled`) {
			t.Fatal("GIF can be chosen for a banner without a timeline, which the server refuses")
		}
		if !jsBool(t, ctx, `document.querySelector('input[name="render-format"][value="png"]').checked`) {
			t.Fatal("PNG is not the choice")
		}
	})

	t.Run("timeline_allows_gif", func(t *testing.T) {
		fs := newFakeRenderStudio(t)
		_, base := startRenderServer(t, fs)
		ctx := browserFor(t)
		openPage(t, ctx, base+"/")
		waitJS(t, ctx, `!document.getElementById("scrubber").hidden`)
		if jsBool(t, ctx, `document.querySelector('input[name="render-format"][value="gif"]').disabled`) {
			t.Fatal("GIF disabled for a banner with a timeline")
		}
	})

	t.Run("rendering_shows_dots_and_hides_old_result", func(t *testing.T) {
		fs := newFakeRenderStudio(t)
		_, base := startRenderServer(t, fs)
		ctx := browserFor(t)
		openPage(t, ctx, base+"/")
		if err := chromedp.Run(ctx, chromedp.Evaluate(`localStorage.setItem("taufinity.preview.renderConfirmed","1")`, nil)); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(ctx, chromedp.Click("#render-button", chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
		waitJS(t, ctx, `document.getElementById("render-result").hidden === false`)
		// Second render: in the same task as the click, before any answer.
		got := jsString(t, ctx, `(function(){
			document.getElementById("render-button").click();
			var b = document.getElementById("render-button");
			return [String(document.getElementById("render-result").hidden), String(!!b.querySelector(".dots")), String(b.disabled),
			        getComputedStyle(b.querySelector(".dots"), "::after").animationName].join(",");
		})()`)
		if got != "true,true,true,dots" {
			t.Fatalf("while rendering: result hidden, dots, disabled, animation = %q, want true,true,true,dots", got)
		}
		waitJS(t, ctx, `document.getElementById("render-result").hidden === false`)
		if got := jsString(t, ctx, `document.getElementById("render-button").textContent`); got != "Echte render (normale prijs)" {
			t.Fatalf("button after render = %q", got)
		}
	})
}

// The charge line follows the clock: after session_until it says the next
// change starts a new paid hour, instead of a time that has passed.
func TestPreviewPageChargeExpiresBrowser(t *testing.T) {
	for _, tc := range []struct{ name, until, want string }{
		{"active", "2099-01-01T00:00:00Z", "1 credit voor deze template, sessie tot"},
		{"expired", "2020-01-01T00:00:00Z", "sessie verlopen: de volgende wijziging start een nieuw uur (1 credit)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rep := threeRows()
			rep.Charge = &bannerCharge{Credits: 1, SessionUntil: tc.until}
			files := map[string]string{"rows/0/index.html": "<html></html>", "rows/1/index.html": "<html></html>", "rows/2/index.html": "<html></html>"}
			_, base, _ := startBanner(t, rep, files)
			ctx := browserFor(t)
			openPage(t, ctx, base+"/")
			if got := jsString(t, ctx, `document.getElementById("charge").textContent`); !strings.HasPrefix(got, tc.want) {
				t.Fatalf("charge line = %q, want %q", got, tc.want)
			}
		})
	}
}

// Review findings on the first version of this page.
func TestPreviewPageReviewFindingsBrowser(t *testing.T) {
	t.Run("result_stays_with_its_row", func(t *testing.T) {
		// Render row 2, switch to row 3: the heading said row 3 while the
		// image was row 2's.
		fs := newFakeRenderStudio(t)
		rep := renderReport()
		rep.Rows = append(rep.Rows, bannerRow{Index: 1, Label: "leeg", Values: map[string]string{"titel": ""}})
		s, base, _ := startBanner(t, rep, map[string]string{"rows/0/index.html": timelineBanner, "rows/1/index.html": timelineBanner})
		s.Renderer = fs.renderer(templateDir(t, map[string]string{"index.html": "<html></html>"}))
		ctx := browserFor(t)
		openPage(t, ctx, base+"/")
		if err := chromedp.Run(ctx, chromedp.Evaluate(`localStorage.setItem("taufinity.preview.renderConfirmed","1")`, nil)); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(ctx, chromedp.Click("#render-button", chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
		waitJS(t, ctx, `!!document.getElementById("render-image")`)
		selectRow(t, ctx, "1")
		if !jsBool(t, ctx, `document.getElementById("render-result").hidden && !document.getElementById("render-image")`) {
			t.Fatal("row 0's render shows under row 1")
		}
		if got := jsString(t, ctx, `document.getElementById("render-title").textContent`); got != "Echte render van leeg" {
			t.Fatalf("title = %q", got)
		}
		selectRow(t, ctx, "0")
		if !jsBool(t, ctx, `!!document.getElementById("render-image")`) {
			t.Fatal("row 0's own render is gone after switching back")
		}
	})

	t.Run("gif_switches_off_for_a_row_without_timeline", func(t *testing.T) {
		rep := renderReport()
		rep.Rows = append(rep.Rows, bannerRow{Index: 1, Label: "leeg"})
		_, base, _ := startBanner(t, rep, map[string]string{"rows/0/index.html": timelineBanner, "rows/1/index.html": "<html></html>"})
		ctx := browserFor(t)
		openPage(t, ctx, base+"/")
		waitJS(t, ctx, `!document.getElementById("scrubber").hidden`)
		if err := chromedp.Run(ctx, chromedp.Click(`input[name="render-format"][value="gif"]`, chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
		selectRow(t, ctx, "1")
		waitJS(t, ctx, `!document.getElementById("scrubber-note").hidden`)
		if !jsBool(t, ctx, `document.querySelector('input[name="render-format"][value="png"]').checked && document.querySelector('input[name="render-format"][value="gif"]').disabled`) {
			t.Fatal("GIF stays chosen for a row without a timeline")
		}
	})

	t.Run("one_row_after_overview_search_is_a_match", func(t *testing.T) {
		files := map[string]string{"rows/0/index.html": "<html></html>", "rows/1/index.html": "<html></html>", "rows/2/index.html": "<html></html>"}
		_, base, _ := startBanner(t, threeRows(), files)
		ctx := browserFor(t)
		if err := chromedp.Run(ctx, chromedp.Navigate(base+"/#view=all")); err != nil {
			t.Fatal(err)
		}
		waitJS(t, ctx, `document.querySelectorAll("#overview .cell").length === 3`)
		if err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){var s=document.getElementById("row-search");s.value="molen";s.dispatchEvent(new Event("input"));})()`, nil)); err != nil {
			t.Fatal(err)
		}
		if err := chromedp.Run(ctx, chromedp.Click("#view-row", chromedp.ByQuery)); err != nil {
			t.Fatal(err)
		}
		if got := jsString(t, ctx, `document.getElementById("row-picker").value + " " + document.getElementById("banner-frame").getAttribute("src")`); !strings.HasPrefix(got, "2 ") || !strings.Contains(got, "/rows/2/") {
			t.Fatalf("picker value and frame = %q, want the only match, row index 2", got)
		}
		if err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){var s=document.getElementById("row-search");s.value="bestaat niet";s.dispatchEvent(new Event("input"));})()`, nil)); err != nil {
			t.Fatal(err)
		}
		if got := jsString(t, ctx, `document.getElementById("row-picker").options[0].textContent`); !strings.HasPrefix(got, "geen regel bevat") {
			t.Fatalf("picker without a match = %q", got)
		}
	})
}

func selectRow(t *testing.T, ctx chromedpCtx, v string) {
	t.Helper()
	if err := chromedp.Run(ctx, chromedp.Evaluate(`(function(){var s=document.getElementById("row-picker");s.value=`+jsQuote(v)+`;s.dispatchEvent(new Event("change"));})()`, nil)); err != nil {
		t.Fatal(err)
	}
}
