package commands

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPreviewHandlersRecoverFromPanic(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/boom", func(w http.ResponseWriter, r *http.Request) { panic("kapot") })
	mux.HandleFunc("/late", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		panic("na de kop")
	})
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("goed")) })
	srv := httptest.NewServer(recoverPanics(mux))
	defer srv.Close()

	if code, body := httpGet(t, srv.URL+"/boom"); code != 500 || !strings.Contains(body, "interne fout") {
		t.Fatalf("panic: %d %q", code, body)
	}
	if code, _ := httpGet(t, srv.URL+"/late"); code != http.StatusAccepted {
		t.Fatalf("late panic must keep the written status, got %d", code)
	}
	if code, body := httpGet(t, srv.URL+"/ok"); code != 200 || body != "goed" {
		t.Fatalf("next request: %d %q", code, body)
	}
}

// The real preview handler is wrapped: a nil client in the renderer panics in
// the render handler, and the server keeps serving.
func TestPreviewServerHandlerIsRecovered(t *testing.T) {
	s, base := startRenderServer(t, newFakeRenderStudio(t))
	s.Renderer.Client = nil
	rep := renderReport()
	rep.Rows[0].Values = map[string]string{"titel": "x"}
	s.Swap(&bannerResult{Dir: s.dir, Report: rep})
	code, _ := postRender(t, base, s.token, "", `{"row":0,"format":"png"}`)
	if code != 500 {
		t.Fatalf("render with panic: %d", code)
	}
	if code, _ := httpGet(t, base+"/api/state"); code != 200 {
		t.Fatalf("state after panic: %d", code)
	}
}
