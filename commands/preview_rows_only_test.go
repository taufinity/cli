package commands

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"testing"
)

// rawStatus sends the request line exactly as given, so an encoded ".." reaches
// the server as a client would send it, unnormalised.
func rawStatus(t *testing.T, origin, target string) int {
	t.Helper()
	u, err := url.Parse(origin)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", target, u.Host)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// The banner origin serves the files of a row and nothing else of the fill
// folder, however the path is encoded.
func TestBannerServesRowFilesOnly(t *testing.T) {
	s, _, _ := startBanner(t, simpleReport(), map[string]string{
		"rows/0/index.html": "<html></html>",
		"rows/0/a/b.css":    "b{}",
		"report.json":       `{"rows":"customer data"}`,
		"secret.txt":        "top level",
	})
	key := "/" + s.bannerKey
	for _, p := range []string{
		"/rows/%2e%2e/report.json",
		"/rows/..%2freport.json",
		"/rows/0/..%2f..%2fsecret.txt",
		"/rows/0/%2e%2e/%2e%2e/report.json",
		"/rows/%2e%2e/%2e%2e/etc/passwd",
		"/rows/x/index.html",
		"/rows/0",
	} {
		if code := rawStatus(t, s.BannerOrigin(), key+p); code == http.StatusOK {
			t.Errorf("%s: 200; only files of a row may be served", p)
		}
	}
	for _, p := range []string{"/rows/0/index.html", "/rows/0/a/b.css"} {
		if code := rawStatus(t, s.BannerOrigin(), key+p); code != http.StatusOK {
			t.Errorf("%s: %d, want 200", p, code)
		}
	}
}
