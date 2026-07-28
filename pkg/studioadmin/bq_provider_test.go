package studioadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// The allowed_tables JSON-string round-trip is the fidelity-critical bit: the API
// stores it as a stringified array, and a wrong decode/encode causes a perpetual
// Terraform diff. This test drives GetBQProvider against a stub returning the
// stringified form and asserts the slice comes back correct.
func TestGetBQProvider_AllowedTablesRoundTrip(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/bq-providers/9" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"id": 9,
			"name": "VP BQ",
			"endpoint_url": "voorpositiviteit.studio_reporting",
			"allowed_tables": "[\"a\",\"b\",\"c\"]",
			"is_enabled": true
		}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "tok", "")
	p, err := c.GetBQProvider(context.Background(), 9)
	if err != nil {
		t.Fatalf("GetBQProvider: %v", err)
	}
	if got, want := p.AllowedTables, []string{"a", "b", "c"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("allowed_tables = %v, want %v", got, want)
	}
	if !p.Enabled || p.ID != 9 {
		t.Fatalf("unexpected fields: %+v", p)
	}
}

// An empty allowed_tables must decode to an empty (non-nil) slice, not error.
func TestGetBQProvider_EmptyAllowedTables(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"allowed_tables":""}`))
	}))
	defer srv.Close()
	p, err := New(srv.URL, "tok", "").GetBQProvider(context.Background(), 1)
	if err != nil {
		t.Fatalf("GetBQProvider: %v", err)
	}
	if len(p.AllowedTables) != 0 {
		t.Fatalf("want empty, got %v", p.AllowedTables)
	}
}

// writeAllowedTables must send the slice re-encoded as a JSON-array STRING.
func TestUpdateBQProvider_EncodesAllowedTablesAsString(t *testing.T) {
	var bqBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/admin/bq-providers/9" {
			_ = json.NewDecoder(r.Body).Decode(&bqBody)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := New(srv.URL, "tok", "").UpdateBQProvider(context.Background(), &BQProvider{ID: 9, AllowedTables: []string{"x", "y"}})
	if err != nil {
		t.Fatalf("UpdateBQProvider: %v", err)
	}
	got, ok := bqBody["allowed_tables"].(string)
	if !ok {
		t.Fatalf("allowed_tables not a string: %T %v", bqBody["allowed_tables"], bqBody["allowed_tables"])
	}
	if got != `["x","y"]` {
		t.Fatalf("allowed_tables string = %q, want %q", got, `["x","y"]`)
	}
}

// The org header must be numeric X-Organization-ID for a numeric org, slug otherwise.
func TestClient_OrgHeaderSelection(t *testing.T) {
	cases := []struct {
		org        string
		wantHeader string
		wantValue  string
	}{
		{"3", "X-Organization-Id", "3"},
		{"voorpositiviteit", "X-Organization-Slug", "voorpositiviteit"},
	}
	for _, tc := range cases {
		t.Run(tc.org, func(t *testing.T) {
			var gotID, gotSlug string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotID = r.Header.Get("X-Organization-ID")
				gotSlug = r.Header.Get("X-Organization-Slug")
				_, _ = w.Write([]byte(`{}`))
			}))
			defer srv.Close()
			_ = New(srv.URL, "tok", tc.org).Get(context.Background(), "/whatever", nil)
			if tc.wantHeader == "X-Organization-Id" && gotID != tc.wantValue {
				t.Fatalf("X-Organization-ID = %q, want %q", gotID, tc.wantValue)
			}
			if tc.wantHeader == "X-Organization-Slug" && gotSlug != tc.wantValue {
				t.Fatalf("X-Organization-Slug = %q, want %q", gotSlug, tc.wantValue)
			}
		})
	}
}

// A non-2xx response must surface as a structured APIError.
func TestClient_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"nope"}`))
	}))
	defer srv.Close()
	err := New(srv.URL, "tok", "").Get(context.Background(), "/x", nil)
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", apiErr.Status)
	}
}
