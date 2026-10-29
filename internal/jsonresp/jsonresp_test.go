package jsonresp

import (
	"errors"
	"strings"
	"testing"
)

const page = `<!DOCTYPE html>
<html><head><title>
  502 Bad   Gateway &amp; friends
</title></head><body>SECRET-LOOKING-BODY-CONTENT</body></html>`

func TestHTMLGivesShortErrorWithTitleNotBody(t *testing.T) {
	err := Check("https://studio.example/api/x?code=abc", 502, "text/html; charset=utf-8", []byte(page))
	var nj *NonJSONError
	if !errors.As(err, &nj) {
		t.Fatalf("want *NonJSONError, got %v", err)
	}
	want := `expected JSON from https://studio.example/api/x, got text/html (HTTP 502): page title "502 Bad Gateway & friends"`
	if err.Error() != want {
		t.Fatalf("message = %q\nwant      %q", err.Error(), want)
	}
	if strings.Contains(err.Error(), "SECRET-LOOKING-BODY-CONTENT") || strings.Contains(err.Error(), "code=abc") {
		t.Fatal("the message carries the body or the query string")
	}
}

func TestHTMLWithoutContentTypeIsSniffed(t *testing.T) {
	if Check("https://x/api", 200, "", []byte("  \n<html><body>hi</body></html>")) == nil {
		t.Fatal("an HTML body without a content type was not recognised")
	}
}

func TestJSONAndEmptyPassThrough(t *testing.T) {
	for _, tc := range []struct {
		ct   string
		body string
	}{
		{"application/json", `{"ok":true}`},
		{"application/json", `<html>`}, // declared JSON: the decoder reports it
		{"", ``},
		{"text/plain", `not found`},
	} {
		if err := Check("https://x/api", 200, tc.ct, []byte(tc.body)); err != nil {
			t.Errorf("%q %q: unexpected %v", tc.ct, tc.body, err)
		}
	}
}
