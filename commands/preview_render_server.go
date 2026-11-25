package commands

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// renderTokenPlaceholder is replaced by the per-process token in the page HTML.
const renderTokenPlaceholder = "__PREVIEW_TOKEN__"

// The three render deadlines nest, shortest first:
//
//	Studio's own cap (renderTimeout) < the client timeout (bannerRenderClientTimeout,
//	in preview.go) < the local handler's deadline (renderHandlerTimeout).
//
// The client timeout is what ends a render that Studio answers too slowly, with
// its own message. The handler's deadline is only a backstop above it, so it
// must never fire first: then the client timeout would be dead code and a
// render that Studio answers just under its cap would be cut off locally.
const (
	renderTimeout        = 5 * time.Minute
	renderHandlerMargin  = 15 * time.Second
	renderHandlerTimeout = bannerRenderClientTimeout + renderHandlerMargin
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// renderResponse is what the page gets after a real render.
type renderResponse struct {
	URL    string `json:"url,omitempty"` // /render/<n>, empty when nothing was rendered
	Status string `json:"status"`
	Format string `json:"format"`
	Bytes  int64  `json:"bytes"`
	Notes  string `json:"notes"`
	JobID  string `json:"job_id,omitempty"`
}

// guardPagePost lets only the page itself through: POST, same origin, and the
// per-process token. It answers the refusal and returns false otherwise.
func (s *bannerServer) guardPagePost(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONError(w, http.StatusMethodNotAllowed, "alleen POST is toegestaan")
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host {
		writeJSONError(w, http.StatusForbidden, "verzoek van een andere herkomst geweigerd")
		return false
	}
	got := r.Header.Get("X-Preview-Token")
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
		writeJSONError(w, http.StatusForbidden, "ontbrekende of onjuiste sleutel; laad de pagina opnieuw")
		return false
	}
	return true
}

// renderConfirmed reads the confirmation of this template folder (once, then
// from memory).
func (s *bannerServer) renderConfirmed() bool {
	s.confirmMu.Lock()
	defer s.confirmMu.Unlock()
	if !s.confirmLoaded && s.TemplateDir != "" {
		if ok, err := readRenderConfirmed(s.TemplateDir); err == nil {
			s.confirmed = ok
			s.confirmLoaded = true
		}
	}
	return s.confirmed
}

// confirmRender remembers the confirmation in the template folder.
func (s *bannerServer) confirmRender() error {
	s.confirmMu.Lock()
	defer s.confirmMu.Unlock()
	if s.TemplateDir != "" {
		if err := writeRenderConfirmed(s.TemplateDir); err != nil {
			return err
		}
	}
	s.confirmed, s.confirmLoaded = true, true
	return nil
}

// handleRenderConfirm is what the page calls when the user confirms the
// "echte render kost credits" dialog.
func (s *bannerServer) handleRenderConfirm(w http.ResponseWriter, r *http.Request) {
	if !s.guardPagePost(w, r) {
		return
	}
	if err := s.confirmRender(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "de bevestiging kon niet worden bewaard: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"render_confirmed": true})
}

// handleRender asks Studio for a real render, which costs credits. It takes
// POST only, from the page itself (token and origin), one at a time.
func (s *bannerServer) handleRender(w http.ResponseWriter, r *http.Request) {
	if !s.guardPagePost(w, r) {
		return
	}
	if s.Renderer == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "echte render is niet beschikbaar in deze sessie")
		return
	}
	if !s.renderBusy.CompareAndSwap(false, true) {
		writeJSONError(w, http.StatusConflict, "er loopt al een echte render; wacht tot die klaar is")
		return
	}
	defer s.renderBusy.Store(false)

	var in struct {
		Row    int    `json:"row"`
		Format string `json:"format"`
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 4096))
	if err := dec.Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "ongeldig verzoek")
		return
	}
	s.mu.RLock()
	rep := s.report
	s.mu.RUnlock()

	deadline := s.renderDeadline
	if deadline <= 0 {
		deadline = renderHandlerTimeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), deadline)
	defer cancel()
	file, err := s.nextRenderFile(in.Format)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := s.Renderer.Render(ctx, rep, in.Row, in.Format, file)
	if err != nil {
		var re *renderError
		switch {
		case errors.As(err, &re) && re.Status == http.StatusBadRequest:
			writeJSONError(w, http.StatusBadRequest, re.Msg)
		case errors.As(err, &re) && re.Status == http.StatusUnprocessableEntity:
			writeJSONError(w, http.StatusUnprocessableEntity, re.Msg)
		case errors.As(err, &re):
			writeJSONError(w, http.StatusBadGateway, re.Msg)
		case errors.Is(err, context.DeadlineExceeded):
			writeJSONError(w, http.StatusGatewayTimeout, "de echte render duurde te lang en is afgebroken")
		default:
			writeJSONError(w, http.StatusBadGateway, "echte render mislukt: "+err.Error())
		}
		return
	}
	resp := renderResponse{Status: out.Status, Format: out.Format, Bytes: out.Bytes, Notes: out.Notes, JobID: out.JobID}
	if out.File != "" {
		s.renderMu.Lock()
		s.renders = append(s.renders, out)
		idx := len(s.renders) - 1
		s.renderMu.Unlock()
		resp.URL = "/render/" + strconv.Itoa(idx)
	}
	writeJSON(w, http.StatusOK, resp)
}

// nextRenderFile makes sure the render folder exists and names the next file.
func (s *bannerServer) nextRenderFile(format string) (string, error) {
	s.renderMu.Lock()
	defer s.renderMu.Unlock()
	if s.renderDir == "" {
		dir, err := newBannerTempDir()
		if err != nil {
			return "", err
		}
		s.renderDir = dir
	}
	n := len(s.renders)
	return filepath.Join(s.renderDir, fmt.Sprintf("render-%d-%d.%s", n, time.Now().UnixNano(), format)), nil
}

// handleRenderFile serves a rendered file to the page: /render/<n>.
func (s *bannerServer) handleRenderFile(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/render/"))
	if err != nil || n < 0 {
		http.NotFound(w, r)
		return
	}
	s.renderMu.Lock()
	var out *renderOutcome
	if n < len(s.renders) {
		out = s.renders[n]
	}
	s.renderMu.Unlock()
	if out == nil {
		http.NotFound(w, r)
		return
	}
	data, err := os.ReadFile(out.File)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := "image/png"
	if out.Format == "gif" {
		ct = "image/gif"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}
