package commands

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"

	"github.com/taufinity/cli/internal/api"
	"github.com/taufinity/cli/internal/archive"
)

const (
	bannerEndpoint   = "/api/creatives/banner-preview"
	bannerMaxZip     = 32 << 20  // server limit for the template ZIP
	bannerMaxUnpack  = 512 << 20 // sanity cap on the answer, zip bomb guard
	bannerMaxOneFile = 128 << 20
)

// bannerName is a field name. The contract lists plain strings; an object with
// a "name" is accepted too so a richer server answer does not break the CLI.
type bannerName string

func (n *bannerName) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*n = bannerName(s)
		return nil
	}
	var o struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	*n = bannerName(o.Name)
	return nil
}

type bannerRowError struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Message string `json:"message"`
}

type bannerRow struct {
	Index    int               `json:"index"`
	Label    string            `json:"label"`
	Filled   []bannerName      `json:"filled"`
	Unfilled []bannerName      `json:"unfilled"`
	Unused   []bannerName      `json:"unused"`
	Warnings []string          `json:"warnings"`
	Error    *bannerRowError   `json:"error"`
	Values   map[string]string `json:"values,omitempty"` // optional extension, used by --init
}

type bannerReport struct {
	Entry    string       `json:"entry"`
	Width    int          `json:"width"`
	Height   int          `json:"height"`
	Fields   []bannerName `json:"fields"`
	Optional []bannerName `json:"optional"`
	Rows     []bannerRow  `json:"rows"`
}

// bannerResult is one successful fill: the report and the folder it was
// unpacked into.
type bannerResult struct {
	Dir    string
	Report *bannerReport
}

// bannerSource knows where a preview comes from and can fill it any number of
// times. One api.Client (one HTTP client) is reused for every refill.
type bannerSource struct {
	Client   *api.Client
	Dir      string
	DataFlag string
	Row      int
	NoRows   bool // --init: never send rows
}

// buildBannerRequest makes the multipart body: the zipped folder and, when
// there are rows, their JSON.
func buildBannerRequest(dir string, rows []map[string]string) (*bytes.Buffer, string, error) {
	var zipBuf bytes.Buffer
	if _, err := archive.CreateZip(dir, &zipBuf); err != nil {
		return nil, "", fmt.Errorf("map inpakken: %w", err)
	}
	if zipBuf.Len() > bannerMaxZip {
		return nil, "", fmt.Errorf("de map is %d MB na inpakken; het maximum is 32 MB", zipBuf.Len()>>20)
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="template"; filename="template.zip"`)
	h.Set("Content-Type", "application/zip")
	part, err := w.CreatePart(h)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(zipBuf.Bytes()); err != nil {
		return nil, "", err
	}
	if len(rows) > 0 {
		js, err := json.Marshal(rows)
		if err != nil {
			return nil, "", err
		}
		if err := w.WriteField("rows", string(js)); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &body, w.FormDataContentType(), nil
}

// bannerHTTPError turns an error answer into one Dutch message. A 429 shows
// the server's own (Dutch) message as is.
func bannerHTTPError(status int, body []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	msg := ""
	if json.Unmarshal(body, &e) == nil {
		msg = strings.TrimSpace(e.Error)
	}
	if msg == "" {
		if t := strings.TrimSpace(string(body)); t != "" && len(t) <= 300 && !strings.HasPrefix(t, "<") {
			msg = t
		}
	}
	switch status {
	case http.StatusBadRequest:
		if msg == "" {
			msg = "onbekende fout"
		}
		return fmt.Errorf("de sjabloon kan niet worden gelezen: %s", msg)
	case http.StatusRequestEntityTooLarge:
		return errors.New("de map is te groot (maximaal 32 MB)")
	case http.StatusTooManyRequests:
		if msg == "" {
			msg = "te veel verzoeken, probeer het straks opnieuw"
		}
		return errors.New(msg)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("geen toegang (HTTP %d): log in met 'taufinity auth login'", status)
	}
	if msg == "" {
		msg = "onbekende fout"
	}
	return fmt.Errorf("de server antwoordde met HTTP %d: %s", status, msg)
}

// unpackBannerZip extracts the answer into dest, refusing paths that escape it.
func unpackBannerZip(data []byte, dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("het antwoord van de server is geen geldige zip: %w", err)
	}
	var total int64
	root := filepath.Clean(dest) + string(os.PathSeparator)
	for _, f := range zr.File {
		target := filepath.Join(dest, filepath.FromSlash(f.Name))
		if !strings.HasPrefix(target, root) {
			return fmt.Errorf("onveilig pad in het antwoord: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			rc.Close()
			return err
		}
		n, err := io.Copy(out, io.LimitReader(rc, bannerMaxOneFile+1))
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
		total += n
		if n > bannerMaxOneFile || total > bannerMaxUnpack {
			return errors.New("het antwoord van de server is te groot")
		}
	}
	return nil
}

func readBannerReport(dir string) (*bannerReport, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		return nil, errors.New("het antwoord van de server bevat geen report.json")
	}
	var r bannerReport
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("report.json is onleesbaar: %w", err)
	}
	if r.Entry == "" || len(r.Rows) == 0 {
		return nil, errors.New("report.json mist het startbestand of de regels")
	}
	return &r, nil
}

// Fill reads the data file, zips the folder, calls the endpoint and unpacks
// the answer into a fresh temp folder. The caller owns result.Dir.
func (s *bannerSource) Fill(ctx context.Context) (*bannerResult, error) {
	var rows []map[string]string
	if !s.NoRows {
		var err error
		rows, _, err = loadBannerRows(s.Dir, s.DataFlag, s.Row)
		if err != nil {
			return nil, err
		}
	}
	body, ct, err := buildBannerRequest(s.Dir, rows)
	if err != nil {
		return nil, err
	}
	resp, err := s.Client.PostMultipart(ctx, bannerEndpoint, body, ct)
	if err != nil {
		return nil, fmt.Errorf("geen verbinding met Studio: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, bannerHTTPError(resp.StatusCode, resp.Body)
	}
	dir, err := os.MkdirTemp("", "taufinity-preview-*")
	if err != nil {
		return nil, err
	}
	if err := unpackBannerZip(resp.Body, dir); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	rep, err := readBannerReport(dir)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return &bannerResult{Dir: dir, Report: rep}, nil
}
