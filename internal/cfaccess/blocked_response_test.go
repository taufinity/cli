package cfaccess

// A blocked request's message has to say what the response was, not only that a
// token was refused. On 2026-11-05 a provision apply reported the Access token as
// refused even after a fresh read from Secret Manager, while the token was
// correct: reads with the same token through the same client succeeded in that
// run and only the POST was turned away. The message sent the reader to the
// Access policy and to Secret Manager fingerprints, neither of which was the
// problem. The method, status, redirect host and content type are known where the
// error is built, and they are what tells those two cases apart.

import (
	"net/http"
	"strings"
	"testing"
)

func TestBlockedErrorNamesTheResponse(t *testing.T) {
	t.Run("redirect to the access login", func(t *testing.T) {
		f := newFixture(t, accessRedirect)
		_, err := f.get(t, "https://"+StagingHost+"/api/x")
		be := AsBlocked(err)
		if be == nil {
			t.Fatalf("want *BlockedError, got %v", err)
		}
		if be.Method != http.MethodGet {
			t.Errorf("Method = %q, want GET", be.Method)
		}
		if be.Status != http.StatusFound {
			t.Errorf("Status = %d, want 302", be.Status)
		}
		if be.LocationHost != "taufinity.cloudflareaccess.com" {
			t.Errorf("LocationHost = %q, want the Access login host", be.LocationHost)
		}
		msg := be.Error()
		for _, want := range []string{"GET", "answered 302", "taufinity.cloudflareaccess.com"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message does not mention %q: %s", want, msg)
			}
		}
	})

	t.Run("html where json was expected", func(t *testing.T) {
		f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<!DOCTYPE html><html>Forbidden</html>"))
		})
		_, err := f.get(t, "https://"+StagingHost+"/api/x")
		be := AsBlocked(err)
		if be == nil {
			t.Fatalf("want *BlockedError, got %v", err)
		}
		if be.Status != http.StatusForbidden {
			t.Errorf("Status = %d, want 403", be.Status)
		}
		if be.ContentType != "text/html" {
			t.Errorf("ContentType = %q, want text/html without the charset", be.ContentType)
		}
		if be.LocationHost != "" {
			t.Errorf("LocationHost = %q, want empty when there is no redirect", be.LocationHost)
		}
		msg := be.Error()
		if !strings.Contains(msg, "answered 403") || !strings.Contains(msg, "text/html") {
			t.Errorf("message does not describe the response: %s", msg)
		}
	})

	// The detail is appended, not substituted: the existing advice about the
	// token has to survive, because for a genuinely refused token it is right.
	t.Run("keeps the advice it already gave", func(t *testing.T) {
		f := newFixture(t, accessRedirect)
		_, err := f.get(t, "https://"+StagingHost+"/api/x")
		be := AsBlocked(err)
		if be == nil {
			t.Fatalf("want *BlockedError, got %v", err)
		}
		if !strings.Contains(be.Error(), "was refused (rotated or revoked?)") {
			t.Errorf("the token advice was dropped: %s", be.Error())
		}
	})

	// Nothing that could carry a credential, and nothing unbounded.
	t.Run("carries no body and no headers", func(t *testing.T) {
		f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.Header().Set("Set-Cookie", "CF_Authorization=secret-cookie-value")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("<html>sensitive-body-marker</html>"))
		})
		_, err := f.get(t, "https://"+StagingHost+"/api/x")
		be := AsBlocked(err)
		if be == nil {
			t.Fatalf("want *BlockedError, got %v", err)
		}
		msg := be.Error()
		for _, leak := range []string{"sensitive-body-marker", "secret-cookie-value", "CF_Authorization", fakeID, fakeSecret} {
			if strings.Contains(msg, leak) {
				t.Errorf("message leaks %q: %s", leak, msg)
			}
		}
	})

	// An error built without a response still reads as it did before.
	// (The success case below is why this file exists at all.)
	t.Run("no response recorded reads as before", func(t *testing.T) {
		be := &BlockedError{Host: StagingHost, TokenSent: true, Source: SourceGcloud}
		msg := be.Error()
		// The message already contains "(rotated or revoked?)", so the test is
		// that nothing was appended rather than that no bracket appears.
		if strings.Contains(msg, "()") || strings.Contains(msg, "answered") {
			t.Errorf("an error with nothing recorded should gain no response detail: %s", msg)
		}
		if !strings.Contains(msg, "was refused (rotated or revoked?)") {
			t.Errorf("unexpected message: %s", msg)
		}
	})
}
