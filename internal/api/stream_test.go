package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostStream(t *testing.T) {
	ctx := context.Background()

	t.Run("bearer_and_org_header", func(t *testing.T) {
		isolatedHome(t)
		t.Setenv(APIKeyEnv, "")
		var got http.Header
		var gotBody string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
			b, _ := io.ReadAll(r.Body)
			gotBody = string(b)
			_, _ = w.Write([]byte("ok\n"))
		}))
		defer srv.Close()
		seedSession(t, srv.URL, newAccess, newRefresh)
		c := New(srv.URL)
		c.SetOrg("12")
		resp, err := c.PostStream(ctx, "/api/x", []byte(`{"a":1}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if got.Get("Authorization") != "Bearer "+newAccess || got.Get("X-Organization-ID") != "12" ||
			got.Get("Content-Type") != "application/json" || got.Get("X-API-Key") != "" || gotBody != `{"a":1}` {
			t.Fatalf("headers %v body %q", got, gotBody)
		}
	})

	t.Run("api_key_env_wins", func(t *testing.T) {
		isolatedHome(t)
		t.Setenv(APIKeyEnv, "key-from-env") // gitleaks:allow -- clearly fake test value
		var got http.Header
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got = r.Header.Clone()
		}))
		defer srv.Close()
		seedSession(t, srv.URL, newAccess, newRefresh) // a stored login must lose
		resp, err := New(srv.URL).PostStream(ctx, "/api/x", []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got.Get("X-API-Key") != "key-from-env" || got.Get("Authorization") != "" { // gitleaks:allow -- clearly fake test value
			t.Fatalf("headers %v", got)
		}
	})

	t.Run("renews_once_on_401", func(t *testing.T) {
		isolatedHome(t)
		t.Setenv(APIKeyEnv, "")
		s := newRotatedServer(t)
		seedSession(t, s.srv.URL, oldAccess, oldRefresh)
		resp, err := New(s.srv.URL).PostStream(ctx, "/api/x", []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || s.refreshes.Load() != 1 || s.apiHits.Load() != 2 {
			t.Fatalf("status=%d refreshes=%d hits=%d", resp.StatusCode, s.refreshes.Load(), s.apiHits.Load())
		}
	})

	t.Run("once_variant_never_replays", func(t *testing.T) {
		isolatedHome(t)
		t.Setenv(APIKeyEnv, "")
		s := newRotatedServer(t)
		seedSession(t, s.srv.URL, oldAccess, oldRefresh)
		resp, err := New(s.srv.URL).PostStreamOnce(ctx, "/api/x", []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 || s.apiHits.Load() != 1 {
			t.Fatalf("status=%d hits=%d, want one 401", resp.StatusCode, s.apiHits.Load())
		}
	})

	t.Run("debug_never_logs_any_part_of_the_bearer_token", func(t *testing.T) {
		isolatedHome(t)
		t.Setenv(APIKeyEnv, "")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}))
		defer srv.Close()
		const tok = "stored-login-token-abcdefghijklmnop" // gitleaks:allow -- clearly fake test value
		seedSession(t, srv.URL, tok, newRefresh)
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(prev)
		c := New(srv.URL)
		c.SetDebug(true)
		if _, err := c.GetWithKeyOrAuth(ctx, "/api/code-review/settings"); err != nil {
			t.Fatal(err)
		}
		if out := logs.String(); strings.Contains(out, "stored-login") || strings.Contains(out, tok[:6]) || !strings.Contains(out, "redacted") {
			t.Errorf("debug output:\n%s", out)
		}
	})

	t.Run("second_401_is_returned", func(t *testing.T) {
		isolatedHome(t)
		t.Setenv(APIKeyEnv, "")
		s := newRotatedServer(t)
		s.accept = ""
		seedSession(t, s.srv.URL, oldAccess, oldRefresh)
		resp, err := New(s.srv.URL).PostStream(ctx, "/api/x", []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 || s.apiHits.Load() != 2 || s.refreshes.Load() != 1 {
			t.Fatalf("status=%d hits=%d refreshes=%d", resp.StatusCode, s.apiHits.Load(), s.refreshes.Load())
		}
	})

	for _, status := range []int{429, 503} {
		name := map[int]string{429: "no_retry_on_429", 503: "no_retry_on_503"}[status]
		t.Run(name, func(t *testing.T) {
			isolatedHome(t)
			t.Setenv(APIKeyEnv, "")
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":"x"}`))
			}))
			defer srv.Close()
			seedSession(t, srv.URL, newAccess, newRefresh)
			resp, err := New(srv.URL).PostStream(ctx, "/api/x", []byte(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != status || hits.Load() != 1 {
				t.Fatalf("status=%d hits=%d, want one request", resp.StatusCode, hits.Load())
			}
		})
	}

	t.Run("streams_incrementally", func(t *testing.T) {
		isolatedHome(t)
		t.Setenv(APIKeyEnv, "k") // gitleaks:allow -- clearly fake test value
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte("first\n"))
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
			_, _ = w.Write([]byte("second\n"))
		}))
		defer srv.Close()
		resp, err := New(srv.URL).PostStream(ctx, "/api/x", []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		buf := make([]byte, 6)
		done := make(chan error, 1)
		go func() { _, e := io.ReadFull(resp.Body, buf); done <- e }()
		select {
		case e := <-done:
			if e != nil || string(buf) != "first\n" {
				t.Fatalf("read %q err %v", buf, e)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the first line was not readable before the second was sent")
		}
		close(release)
		rest, _ := io.ReadAll(resp.Body)
		if string(rest) != "second\n" {
			t.Fatalf("rest %q", rest)
		}
	})

	t.Run("does_not_follow_redirects", func(t *testing.T) {
		isolatedHome(t)
		t.Setenv(APIKeyEnv, "k") // gitleaks:allow -- clearly fake test value
		var elsewhere atomic.Int32
		other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere.Add(1) }))
		defer other.Close()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/steal", http.StatusTemporaryRedirect)
		}))
		defer srv.Close()
		resp, err := New(srv.URL).PostStream(ctx, "/api/x", []byte(`{"diff":"code"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusTemporaryRedirect || elsewhere.Load() != 0 {
			t.Fatalf("status %d, other host saw %d requests", resp.StatusCode, elsewhere.Load())
		}
	})

	t.Run("debug_redacts_body_and_key", func(t *testing.T) {
		isolatedHome(t)
		t.Setenv(APIKeyEnv, "very-secret-key-value") // gitleaks:allow -- clearly fake test value
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer srv.Close()
		var logs bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(prev)
		c := New(srv.URL)
		c.SetDebug(true)
		body := []byte(`{"diff":"SECRET-SOURCE-LINE","files":{"a":"more source"}}`)
		resp, err := c.PostStream(ctx, "/api/x", body)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if _, err := c.GetWithKeyOrAuth(ctx, "/api/y"); err != nil {
			t.Fatal(err)
		}
		out := logs.String()
		for _, leak := range []string{"SECRET-SOURCE-LINE", "more source", "very-secret-key-value"} {
			if strings.Contains(out, leak) {
				t.Errorf("debug output leaks %q:\n%s", leak, out)
			}
		}
		if !strings.Contains(out, "body_bytes=") {
			t.Errorf("debug output should record the body size:\n%s", out)
		}
	})
}
