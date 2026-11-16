package commands

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The page names a sent row after its line in the data file, the number
// --row takes and the Product Layer shows (header = line 1). The server calls
// the first row it got "regel 1", which is line 2 of the file; showing that
// made "regel 1" in the picker and --row 2 the same banner.
func TestBannerRowsLabelledByFileLine(t *testing.T) {
	sent := func(n int) []bannerRow {
		rows := make([]bannerRow, n)
		for i := range rows {
			rows[i] = bannerRow{Index: i, Label: "regel " + string(rune('1'+i))}
		}
		return rows
	}
	// A quoted value over two lines moves every later row down a line.
	csv := "titel,prijs\nA,1\n\"B\nmet een tweede regel\",2\nC,3\n"

	t.Run("all_rows", func(t *testing.T) {
		f := newFakeBanner(t)
		f.report.Rows = sent(3)
		dir := templateDir(t, map[string]string{"data.csv": csv})
		res, err := f.source(t, dir).Fill(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(res.Dir)
		want := []string{"regel 2", "regel 3", "regel 5"}
		for i, w := range want {
			if got := res.Report.Rows[i].Label; got != w {
				t.Errorf("row %d label = %q, want %q", i, got, w)
			}
		}
	})

	t.Run("one_row_with_flag", func(t *testing.T) {
		f := newFakeBanner(t)
		f.report.Rows = sent(1)
		dir := templateDir(t, map[string]string{"data.csv": csv})
		src := f.source(t, dir)
		src.Row = 5
		res, err := src.Fill(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(res.Dir)
		if got := res.Report.Rows[0].Label; got != "regel 5" {
			t.Errorf("--row 5 shows %q, want regel 5", got)
		}
	})

	t.Run("generated_rows_keep_their_names", func(t *testing.T) {
		f := newFakeBanner(t) // no data file: the server's voorbeeld and leeg
		res, err := f.source(t, templateDir(t, nil)).Fill(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(res.Dir)
		if res.Report.Rows[0].Label != "voorbeeld" || res.Report.Rows[1].Label != "leeg" {
			t.Errorf("labels = %q, %q", res.Report.Rows[0].Label, res.Report.Rows[1].Label)
		}
	})

	t.Run("data_file_in_data_folder", func(t *testing.T) {
		f := newFakeBanner(t)
		f.report.Rows = sent(2)
		dir := templateDir(t, nil)
		os.MkdirAll(filepath.Join(dir, "data"), 0o755)
		os.WriteFile(filepath.Join(dir, "data", "rows.csv"), []byte("titel\n\nA\nB\n"), 0o644)
		res, err := f.source(t, dir).Fill(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(res.Dir)
		// An empty line is not a row; the labels follow the file.
		if a, b := res.Report.Rows[0].Label, res.Report.Rows[1].Label; a != "regel 3" || b != "regel 4" {
			t.Errorf("labels = %q, %q, want regel 3, regel 4", a, b)
		}
	})
}
