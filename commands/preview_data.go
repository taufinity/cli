package commands

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// bannerMaxRows is the most rows one preview request carries (server limit).
const bannerMaxRows = 50

// bannerCSV is a parsed data file. Lines[i] is the line number of Rows[i] as
// the Product Layer counts: the header is line 1, the first data row line 2.
type bannerCSV struct {
	Header []string
	Rows   []map[string]string
	Lines  []int
}

// findBannerDataFile decides which data file a preview uses: the explicit one,
// else the only *.csv in the folder or in data/, else none ("", nil).
func findBannerDataFile(dir, explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err != nil {
			return "", fmt.Errorf("gegevensbestand %s niet gevonden", explicit)
		}
		return explicit, nil
	}
	var found []string
	for _, d := range []string{dir, filepath.Join(dir, "data")} {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".csv") {
				found = append(found, filepath.Join(d, e.Name()))
			}
		}
	}
	sort.Strings(found)
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	}
	names := make([]string, len(found))
	for i, f := range found {
		if rel, err := filepath.Rel(dir, f); err == nil {
			f = rel
		}
		names[i] = f
	}
	return "", fmt.Errorf("meerdere gegevensbestanden gevonden (%s): kies er een met --data <bestand.csv>", strings.Join(names, ", "))
}

// readBannerCSV reads a CSV with a comma or semicolon separator (decided from
// the header line), UTF-8 with an optional BOM. The header row holds the field
// names; every following line becomes one row object. Empty lines are skipped
// but still counted in the line numbers.
func readBannerCSV(path string) (*bannerCSV, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("gegevensbestand lezen: %w", err)
	}
	return parseBannerCSV(raw)
}

func parseBannerCSV(raw []byte) (*bannerCSV, error) {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	header := raw
	if i := bytes.IndexByte(raw, '\n'); i >= 0 {
		header = raw[:i]
	}
	comma := ','
	if bytes.Count(header, []byte(";")) > bytes.Count(header, []byte(",")) {
		comma = ';'
	}
	r := csv.NewReader(bytes.NewReader(raw))
	r.Comma = comma
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = false

	out := &bannerCSV{}
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("gegevensbestand is geen geldige CSV: %w", err)
		}
		line, _ := r.FieldPos(0)
		if out.Header == nil {
			for i := range rec {
				rec[i] = strings.TrimSpace(rec[i])
			}
			out.Header = rec
			continue
		}
		row := make(map[string]string, len(out.Header))
		for i, name := range out.Header {
			if name == "" {
				continue
			}
			v := ""
			if i < len(rec) {
				v = rec[i]
			}
			row[name] = v
		}
		out.Rows = append(out.Rows, row)
		out.Lines = append(out.Lines, line)
	}
	if out.Header == nil {
		return nil, fmt.Errorf("gegevensbestand is leeg")
	}
	return out, nil
}

// selectBannerRows applies --row (0 = all rows). The result is what is sent to
// the server.
func selectBannerRows(d *bannerCSV, row int) ([]map[string]string, error) {
	if row != 0 {
		for i, l := range d.Lines {
			if l == row {
				return []map[string]string{d.Rows[i]}, nil
			}
		}
		last := 1
		if n := len(d.Lines); n > 0 {
			last = d.Lines[n-1]
		}
		return nil, fmt.Errorf("regel %d bestaat niet in het gegevensbestand (regel 1 is de kop, de laatste regel is %d)", row, last)
	}
	if len(d.Rows) == 0 {
		return nil, fmt.Errorf("het gegevensbestand heeft een kop maar geen regels met gegevens")
	}
	if len(d.Rows) > bannerMaxRows {
		return nil, fmt.Errorf("het gegevensbestand heeft %d regels; een voorbeeld toont er maximaal %d. Kies een regel met --row <n> (regel 1 is de kop)", len(d.Rows), bannerMaxRows)
	}
	return d.Rows, nil
}

// loadBannerRows combines the three steps. Rows is nil when there is no data
// file, which makes the server generate its own rows.
func loadBannerRows(dir, dataFlag string, row int) (rows []map[string]string, dataPath string, err error) {
	dataPath, err = findBannerDataFile(dir, dataFlag)
	if err != nil {
		return nil, "", err
	}
	if dataPath == "" {
		if row != 0 {
			return nil, "", fmt.Errorf("--row heeft een gegevensbestand nodig, maar er is geen CSV gevonden (gebruik --data)")
		}
		return nil, "", nil
	}
	d, err := readBannerCSV(dataPath)
	if err != nil {
		return nil, "", err
	}
	rows, err = selectBannerRows(d, row)
	return rows, dataPath, err
}
