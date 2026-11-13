package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strings"

	"github.com/taufinity/cli/internal/api"
	"github.com/taufinity/cli/internal/archive"
	"github.com/taufinity/cli/internal/httpclient"
)

// The Studio endpoints of a real render: upload the template, render a row of
// it, download the file.
const (
	renderUploadEndpoint = "/api/templates/upload"
	renderEndpoint       = "/api/creatives/render"
	renderGIFFrames      = 10
)

// renderError is a failed real render. Status is the HTTP status of the answer
// that refused it (0 when there was none), Msg is Dutch.
type renderError struct {
	Status int
	Msg    string
}

func (e *renderError) Error() string { return e.Msg }

// renderOutcome is what Studio answered to a render, and where the file is.
type renderOutcome struct {
	JobID   string `json:"job_id"`
	TraceID string `json:"trace_id"`
	Format  string `json:"format"`
	Status  string `json:"status"` // succeeded or not_achieved
	Bytes   int64  `json:"bytes"`
	Notes   string `json:"notes"`

	DownloadURL string `json:"download_url,omitempty"`
	File        string `json:"-"` // downloaded file in the preview temp dir; empty without a download
}

// bannerRenderer asks Studio for a real render of one row of the template in
// Dir. Every render uploads the folder anew: the template may have changed.
type bannerRenderer struct {
	Client   *api.Client
	Dir      string
	DataFlag string
	Row      int
}

// serverErrorText is the "error" of a JSON error answer, or a short plain body.
func serverErrorText(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && strings.TrimSpace(e.Error) != "" {
		return strings.TrimSpace(e.Error)
	}
	if t := strings.TrimSpace(string(body)); t != "" && len(t) <= 300 && !strings.HasPrefix(t, "<") {
		return t
	}
	return "onbekende fout"
}

// renderHTTPError turns an error answer of Studio into one Dutch message that
// carries the server's own text.
func renderHTTPError(step string, status int, body []byte) *renderError {
	msg := serverErrorText(body)
	var text string
	switch status {
	case http.StatusBadRequest:
		text = step + ": ongeldig verzoek: " + msg
	case http.StatusUnauthorized, http.StatusForbidden:
		text = fmt.Sprintf("geen toegang (HTTP %d): log in met 'taufinity auth login'", status)
	case http.StatusNotFound:
		text = step + ": niet gevonden: " + msg
	case http.StatusRequestEntityTooLarge:
		text = step + ": te groot: " + msg
	case http.StatusUnprocessableEntity:
		text = "de render is geweigerd: " + msg
	case http.StatusServiceUnavailable:
		text = "Studio is nu niet beschikbaar: " + msg
	default:
		text = fmt.Sprintf("%s: Studio antwoordde met HTTP %d: %s", step, status, msg)
	}
	return &renderError{Status: status, Msg: text}
}

func renderTransportError(step string, err error) error {
	if errors.Is(err, httpclient.ErrResponseTooLarge) {
		return &renderError{Msg: step + ": het antwoord van Studio is te groot"}
	}
	if edgeBlocked(err) {
		return &renderError{Msg: step + ": " + err.Error()}
	}
	return &renderError{Msg: fmt.Sprintf("%s: geen verbinding met Studio: %v", step, err)}
}

// buildRenderUpload makes the multipart body: the template folder as tar.gz in
// field "archive". The file set is the one the preview zips.
func buildRenderUpload(ctx context.Context, dir string) (*bytes.Buffer, string, error) {
	var tgz bytes.Buffer
	if _, err := archive.CreateTarGzWithOptions(ctx, dir, &tgz, archive.ZipOptions{
		MaxFiles: bannerMaxZipFiles,
		MaxBytes: bannerMaxZip,
		Warn:     func(m string) { fmt.Fprintln(os.Stderr, m) },
	}); err != nil {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return nil, "", &renderError{Msg: "map inpakken: " + err.Error()}
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="archive"; filename="template.tar.gz"`)
	h.Set("Content-Type", "application/gzip")
	part, err := w.CreatePart(h)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(tgz.Bytes()); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &body, w.FormDataContentType(), nil
}

// Upload sends the template folder and returns the archive id.
func (r *bannerRenderer) Upload(ctx context.Context) (string, error) {
	body, ct, err := buildRenderUpload(ctx, r.Dir)
	if err != nil {
		return "", err
	}
	resp, err := r.Client.PostMultipart(ctx, renderUploadEndpoint, body, ct)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", renderTransportError("uploaden", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", renderHTTPError("uploaden", resp.StatusCode, resp.Body)
	}
	var up struct {
		ArchiveID string `json:"archive_id"`
	}
	if err := json.Unmarshal(resp.Body, &up); err != nil || up.ArchiveID == "" {
		return "", &renderError{Msg: "uploaden: het antwoord van Studio bevat geen archive_id"}
	}
	return up.ArchiveID, nil
}

// variablesFor is the values of row index keyed by the CSV header names. Without
// a data file the values the server generated for that row are used.
func (r *bannerRenderer) variablesFor(rep *bannerReport, index int) (map[string]string, error) {
	rows, _, err := loadBannerRows(r.Dir, r.DataFlag, r.Row)
	if err != nil {
		return nil, err
	}
	if len(rows) > 0 {
		if index < 0 || index >= len(rows) {
			return nil, &renderError{Msg: fmt.Sprintf("regel %d bestaat niet in het gegevensbestand", index)}
		}
		return rows[index], nil
	}
	for _, row := range rep.Rows {
		if row.Index == index {
			vars := map[string]string{}
			maps.Copy(vars, row.Values)
			return vars, nil
		}
	}
	return nil, &renderError{Msg: fmt.Sprintf("regel %d bestaat niet", index)}
}

// renderRequest is the JSON body of POST /api/creatives/render.
type renderRequest struct {
	ArchiveID string            `json:"archive_id"`
	Width     int               `json:"width"`
	Height    int               `json:"height"`
	Format    string            `json:"format"`
	Frames    int               `json:"frames,omitempty"`
	Variables map[string]string `json:"variables"`
}

// Render uploads the template, asks for a real render of row index in format
// ("png" or "gif") and downloads the file to outFile.
func (r *bannerRenderer) Render(ctx context.Context, rep *bannerReport, index int, format, outFile string) (*renderOutcome, error) {
	if format != "png" && format != "gif" {
		return nil, &renderError{Status: http.StatusBadRequest, Msg: "onbekend formaat: kies PNG of GIF"}
	}
	if rep == nil {
		return nil, &renderError{Msg: "er is nog geen banner ingevuld"}
	}
	vars, err := r.variablesFor(rep, index)
	if err != nil {
		return nil, err
	}
	id, err := r.Upload(ctx)
	if err != nil {
		return nil, err
	}
	req := renderRequest{ArchiveID: id, Width: rep.Width, Height: rep.Height, Format: format, Variables: vars}
	if format == "gif" {
		req.Frames = renderGIFFrames
	}
	resp, err := r.Client.PostJSONWithAuth(ctx, renderEndpoint, req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, renderTransportError("renderen", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, renderHTTPError("renderen", resp.StatusCode, resp.Body)
	}
	var out renderOutcome
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, &renderError{Msg: "renderen: het antwoord van Studio is onleesbaar"}
	}
	if out.Format == "" {
		out.Format = format
	}
	if out.Status != "succeeded" || out.DownloadURL == "" {
		return &out, nil // not_achieved: reported, nothing to show
	}
	if !strings.HasPrefix(out.DownloadURL, "/") {
		return nil, &renderError{Msg: "downloaden: Studio gaf een onverwacht adres"}
	}
	dl, err := r.Client.GetRawWithAuth(ctx, out.DownloadURL)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, renderTransportError("downloaden", err)
	}
	if dl.StatusCode != http.StatusOK {
		return nil, renderHTTPError("downloaden", dl.StatusCode, dl.Body)
	}
	out.File = outFile
	if err := os.WriteFile(out.File, dl.Body, 0o600); err != nil {
		return nil, err
	}
	if out.Bytes == 0 {
		out.Bytes = int64(len(dl.Body))
	}
	return &out, nil
}
