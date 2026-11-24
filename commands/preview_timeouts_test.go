package commands

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/taufinity/cli/internal/api"
)

// Studio's own cap < client timeout < local handler deadline.
func TestRenderTimeoutsLeaveMargin(t *testing.T) {
	if !(renderTimeout < bannerRenderClientTimeout) {
		t.Fatalf("client timeout %v must exceed Studio's cap %v", bannerRenderClientTimeout, renderTimeout)
	}
	if !(bannerRenderClientTimeout < renderHandlerTimeout) {
		t.Fatalf("handler deadline %v must exceed client timeout %v", renderHandlerTimeout, bannerRenderClientTimeout)
	}
	if renderHandlerTimeout-bannerRenderClientTimeout != renderHandlerMargin {
		t.Fatalf("handler deadline is client timeout plus margin")
	}
}

func TestRenderClientTimeoutBeatsHandlerDeadline(t *testing.T) {
	fs := newFakeRenderStudio(t)
	fs.hold = make(chan struct{})
	t.Cleanup(func() { close(fs.hold) })
	s, base := startRenderServer(t, fs)
	c := api.NewRender(fs.srv.URL, 300*time.Millisecond) // small client timeout
	c.SetAuth("test-token")
	s.Renderer.Client = c
	s.renderDeadline = 10 * time.Second // handler deadline far above it

	code, body := postRender(t, base, s.token, "", `{"row":0,"format":"png"}`)
	if code != 502 {
		t.Fatalf("status = %d, body %s", code, body)
	}
	var e map[string]string
	_ = json.Unmarshal([]byte(body), &e)
	if !strings.Contains(e["error"], "Studio antwoordde niet binnen de tijd") {
		t.Fatalf("want the client-timeout message, got %q", e["error"])
	}
	if strings.Contains(body, "deadline") || strings.Contains(body, "duurde te lang") {
		t.Fatalf("handler deadline message leaked: %s", body)
	}
}

func TestRenderHandlerDeadlineIsBackstop(t *testing.T) {
	fs := newFakeRenderStudio(t)
	fs.hold = make(chan struct{})
	t.Cleanup(func() { close(fs.hold) })
	s, base := startRenderServer(t, fs)
	s.renderDeadline = 300 * time.Millisecond // below the client timeout
	code, body := postRender(t, base, s.token, "", `{"row":0,"format":"png"}`)
	if code != 504 || !strings.Contains(body, "duurde te lang") {
		t.Fatalf("got %d %s", code, body)
	}
}
