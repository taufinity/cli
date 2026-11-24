package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRenderUsesTheFilledRowNotTheCurrentCSV(t *testing.T) {
	fs := newFakeRenderStudio(t)
	dir := templateDir(t, map[string]string{"data.csv": "titel\nKoude Haven\n"})
	rep := renderReport()
	rep.Rows[0].Values = map[string]string{"titel": "Koude Haven"} // what the last fill showed
	r := fs.renderer(dir)

	// The CSV changes after the fill, without a refill.
	if err := os.WriteFile(filepath.Join(dir, "data.csv"), []byte("titel\nHete Zomer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Render(context.Background(), rep, 0, "png", filepath.Join(t.TempDir(), "r.png")); err != nil {
		t.Fatal(err)
	}
	vars := fs.renders[0]["variables"].(map[string]any)
	if vars["titel"] != "Koude Haven" {
		t.Fatalf("render got %v, want the filled values", vars)
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
