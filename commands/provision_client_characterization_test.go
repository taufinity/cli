package commands

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Characterization tests: pin provisionClient's transport behaviour BEFORE the
// refactor onto pkg/studioadmin, so the refactor is verifiably behaviour-preserving.

// A write in dry-run mode must NOT hit the network and must return {} / 200.
func TestProvisionClient_DryRunShortCircuitsWrites(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
	}))
	defer srv.Close()

	c := newProvisionClient(srv.URL, "tok", true) // dryRun = true
	body, status, err := c.put("/things/1", []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if hit {
		t.Fatalf("dry-run write must not reach the server")
	}
	if status != 200 || strings.TrimSpace(string(body)) != "{}" {
		t.Fatalf("dry-run write should return {}/200, got %d %q", status, body)
	}
}

// A real write must carry X-API-Key, Content-Type, X-Change-Source=provision, and a
// non-empty User-Agent.
func TestProvisionClient_WriteHeaders(t *testing.T) {
	var apiKey, ctype, changeSrc, ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey = r.Header.Get("X-API-Key")
		ctype = r.Header.Get("Content-Type")
		changeSrc = r.Header.Get("X-Change-Source")
		ua = r.Header.Get("User-Agent")
		w.WriteHeader(200)
	}))
	defer srv.Close()

	c := newProvisionClient(srv.URL, "sekret", false)
	if _, _, err := c.post("/things", []byte(`{}`)); err != nil {
		t.Fatalf("post: %v", err)
	}
	if apiKey != "sekret" {
		t.Errorf("X-API-Key = %q, want sekret", apiKey)
	}
	if ctype != "application/json" {
		t.Errorf("Content-Type = %q", ctype)
	}
	if changeSrc != "provision" {
		t.Errorf("X-Change-Source = %q, want provision", changeSrc)
	}
	if ua == "" {
		t.Errorf("User-Agent must be set")
	}
}

// getForOrg / writeForOrg must send X-Organization-ID.
func TestProvisionClient_OrgHeader(t *testing.T) {
	var getOrg, writeOrg string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			getOrg = r.Header.Get("X-Organization-ID")
		} else {
			writeOrg = r.Header.Get("X-Organization-ID")
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()

	c := newProvisionClient(srv.URL, "tok", false)
	if _, _, err := c.getForOrg("/x", 7); err != nil {
		t.Fatalf("getForOrg: %v", err)
	}
	if _, _, err := c.writeForOrg(http.MethodPut, "/x/1", []byte(`{}`), 7); err != nil {
		t.Fatalf("writeForOrg: %v", err)
	}
	if getOrg != "7" {
		t.Errorf("GET X-Organization-ID = %q, want 7", getOrg)
	}
	if writeOrg != "7" {
		t.Errorf("write X-Organization-ID = %q, want 7", writeOrg)
	}
}

// A plain GET carries X-API-Key and Accept: application/json.
func TestProvisionClient_GetHeaders(t *testing.T) {
	var apiKey, accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey = r.Header.Get("X-API-Key")
		accept = r.Header.Get("Accept")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()

	c := newProvisionClient(srv.URL, "tok", false)
	if _, _, err := c.get("/x"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if apiKey != "tok" {
		t.Errorf("X-API-Key = %q", apiKey)
	}
	if accept != "application/json" {
		t.Errorf("Accept = %q, want application/json", accept)
	}
}
