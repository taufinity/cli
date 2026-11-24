package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real fill (fake Studio), then the CSV changes on disk without a refill, then
// the render endpoint is called for row 0: Studio's render must get the values
// of the fill, which is what the page shows.
func TestRenderUsesTheFilledRowNotTheCurrentCSV(t *testing.T) {
	dir := templateDir(t, map[string]string{"data.csv": "titel\nKoude Haven\n"})
	fb := newFakeBanner(t)
	fb.report = bannerReport{Entry: "index.html", Width: 300, Height: 250, Fields: []bannerName{"titel"},
		Rows: []bannerRow{{Index: 0, Label: "regel 2", Values: map[string]string{"titel": "Koude Haven"}}}}
	res, err := fb.source(t, dir).Fill(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	fs := newFakeRenderStudio(t)
	srv, base, _ := startBanner(t, simpleReport(), map[string]string{"rows/0/index.html": "x"})
	srv.Renderer = fs.renderer(dir)
	srv.Swap(res)

	// The CSV changes after the fill, without a refill.
	if err := os.WriteFile(filepath.Join(dir, "data.csv"), []byte("titel\nHete Zomer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, body := postRender(t, base, srv.token, "", `{"row":0,"format":"png"}`)
	if code != 200 {
		t.Fatalf("render: %d %s", code, body)
	}
	if len(fs.renders) != 1 {
		t.Fatalf("studio saw %d renders", len(fs.renders))
	}
	vars := fs.renders[0]["variables"].(map[string]any)
	if vars["titel"] != "Koude Haven" {
		t.Fatalf("render got %v, want the filled values, not the new CSV", vars)
	}
}

func TestRenderMissingRowInSnapshotIsDutchError(t *testing.T) {
	fs := newFakeRenderStudio(t)
	rep := renderReport()
	rep.Rows[0].Values = map[string]string{"titel": "x"}
	_, err := fs.renderer(templateDir(t, nil)).Render(context.Background(), rep, 5, "png", filepath.Join(t.TempDir(), "r.png"))
	var re *renderError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "bestaat niet (meer)") {
		t.Fatalf("err = %v", err)
	}
	if n, _ := fs.counts(); n != 0 {
		t.Fatalf("uploaded despite missing row")
	}
}

func TestRenderRowWithoutValuesIsRefusedNotReadFromCSV(t *testing.T) {
	fs := newFakeRenderStudio(t)
	dir := templateDir(t, map[string]string{"data.csv": "titel\nnu in de CSV\n"})
	rep := renderReport()
	rep.Rows[0].Values = nil
	_, err := fs.renderer(dir).Render(context.Background(), rep, 0, "png", filepath.Join(t.TempDir(), "r.png"))
	if err == nil || !strings.Contains(err.Error(), "geen waarden") {
		t.Fatalf("err = %v", err)
	}
	if n, r := fs.counts(); n != 0 || r != 0 {
		t.Fatalf("studio called: %d %d", n, r)
	}
}
