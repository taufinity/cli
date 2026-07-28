package studioadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// A null schedule from the API must come back as a nil *string (not "").
func TestGetPlaybook_NullSchedule(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":92,"name":"Auto-Approve","trigger_type":"article_validated","schedule":null,"enabled":true}`))
	}))
	defer srv.Close()
	p, err := New(srv.URL, "tok", "").GetPlaybook(context.Background(), 92)
	if err != nil {
		t.Fatalf("GetPlaybook: %v", err)
	}
	if p.Schedule != nil {
		t.Fatalf("schedule = %v, want nil", *p.Schedule)
	}
	if p.Name != "Auto-Approve" || p.Enabled == nil || !*p.Enabled {
		t.Fatalf("unexpected: %+v", p)
	}
}

// Only user-set fields go on the wire; omitted (nil) fields must NOT be sent so the
// API keeps its default. schedule_paused is never in the PUT (own endpoint).
func TestUpdatePlaybook_OmitsUnsetFields(t *testing.T) {
	var putBody map[string]any
	pausedCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "PUT" && r.URL.Path == "/api/playbooks/92":
			_ = json.NewDecoder(r.Body).Decode(&putBody)
		case r.Method == "POST" && r.URL.Path == "/api/playbooks/92/schedule/pause":
			pausedCalled = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Set only name + enabled + schedule_paused; leave the rest nil.
	err := New(srv.URL, "tok", "").UpdatePlaybook(context.Background(),
		&Playbook{ID: 92, Name: "x", Enabled: boolPtr(false), SchedulePaused: boolPtr(true)})
	if err != nil {
		t.Fatalf("UpdatePlaybook: %v", err)
	}
	if _, present := putBody["schedule_paused"]; present {
		t.Fatalf("schedule_paused must NOT be in the update PUT payload")
	}
	for _, omitted := range []string{"description", "trigger_type", "output_key", "schedule_timezone"} {
		if _, present := putBody[omitted]; present {
			t.Fatalf("unset field %q must be omitted from the PUT, got %v", omitted, putBody[omitted])
		}
	}
	if putBody["enabled"] != false {
		t.Fatalf("enabled = %v, want false (it was set)", putBody["enabled"])
	}
	if !pausedCalled {
		t.Fatalf("pause endpoint was not called")
	}
}

// When schedule_paused is nil, the pause endpoint must NOT be touched.
func TestUpdatePlaybook_NilPauseNotCalled(t *testing.T) {
	pausedCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/playbooks/1/schedule/pause" {
			pausedCalled = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	_ = New(srv.URL, "tok", "").UpdatePlaybook(context.Background(), &Playbook{ID: 1, Name: "x"})
	if pausedCalled {
		t.Fatalf("pause endpoint must not be called when schedule_paused is nil")
	}
}

var _ = strPtr // reserved for future tests
