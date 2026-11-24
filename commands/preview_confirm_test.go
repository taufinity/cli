package commands

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func postConfirm(t *testing.T, s *bannerServer, base string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, base+"/api/render-confirm", nil)
	req.Header.Set("X-Preview-Token", s.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func stateConfirmed(t *testing.T, base string) bool {
	t.Helper()
	_, body := httpGet(t, base+"/api/state")
	return strings.Contains(body, `"render_confirmed":true`)
}

func serverFor(t *testing.T, dir string) (*bannerServer, string) {
	t.Helper()
	s, base, _ := startBanner(t, simpleReport(), map[string]string{"rows/0/index.html": "x"})
	s.TemplateDir = dir
	return s, base
}

func TestRenderConfirmationPersistsPerTemplate(t *testing.T) {
	dir := templateDir(t, map[string]string{"index.html": "<html></html>"})
	s1, base1 := serverFor(t, dir)
	if stateConfirmed(t, base1) {
		t.Fatal("confirmed before the user said so")
	}
	if code := postConfirm(t, s1, base1); code != 200 {
		t.Fatalf("confirm: %d", code)
	}
	if !stateConfirmed(t, base1) {
		t.Fatal("not confirmed after confirming")
	}
	keyBefore, _ := templateKeyFor(dir)

	// a new run: another server, another port, same folder
	s2, base2 := serverFor(t, dir)
	if s1.addr == s2.addr {
		t.Fatal("expected another port")
	}
	if !stateConfirmed(t, base2) {
		t.Fatal("confirmation lost on restart")
	}
	if keyAfter, _ := templateKeyFor(dir); keyAfter != keyBefore || keyAfter == "" {
		t.Fatalf("template key changed: %q -> %q", keyBefore, keyAfter)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, ".taufinity", "preview.json"))
	if !strings.Contains(string(raw), "render_confirmed") || !strings.Contains(string(raw), "template_key") {
		t.Fatalf("preview.json = %s", raw)
	}
}

func TestRenderConfirmationIsNotSharedBetweenFolders(t *testing.T) {
	a := templateDir(t, map[string]string{"index.html": "<html></html>"})
	b := templateDir(t, map[string]string{"index.html": "<html></html>"})
	sa, baseA := serverFor(t, a)
	if code := postConfirm(t, sa, baseA); code != 200 {
		t.Fatalf("confirm: %d", code)
	}
	_, baseB := serverFor(t, b)
	if stateConfirmed(t, baseB) {
		t.Fatal("folder B inherited folder A's confirmation")
	}

	// a copied folder brings the file along but is another folder
	c := templateDir(t, map[string]string{"index.html": "<html></html>"})
	raw, _ := os.ReadFile(filepath.Join(a, ".taufinity", "preview.json"))
	_ = os.MkdirAll(filepath.Join(c, ".taufinity"), 0o755)
	_ = os.WriteFile(filepath.Join(c, ".taufinity", "preview.json"), raw, 0o644)
	_, baseC := serverFor(t, c)
	if stateConfirmed(t, baseC) {
		t.Fatal("a copied folder inherited the confirmation")
	}
}

func TestRenderConfirmEndpointGuards(t *testing.T) {
	s, base := serverFor(t, templateDir(t, nil))
	resp, _ := http.Get(base + "/api/render-confirm")
	if resp.StatusCode != 405 {
		t.Fatalf("GET: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodPost, base+"/api/render-confirm", nil)
	r2, _ := http.DefaultClient.Do(req)
	if r2.StatusCode != 403 || s.renderConfirmed() {
		t.Fatalf("no token: %d", r2.StatusCode)
	}
}
