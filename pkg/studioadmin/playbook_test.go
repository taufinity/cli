package studioadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
	if p.Name != "Auto-Approve" || !p.Enabled {
		t.Fatalf("unexpected: %+v", p)
	}
}

// The update PUT must NOT include schedule_paused (it has its own endpoint), and
// UpdatePlaybook must call the pause endpoint separately.
func TestUpdatePlaybook_PauseIsSeparateEndpoint(t *testing.T) {
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

	err := New(srv.URL, "tok", "").UpdatePlaybook(context.Background(),
		&Playbook{ID: 92, Name: "x", SchedulePaused: true})
	if err != nil {
		t.Fatalf("UpdatePlaybook: %v", err)
	}
	if _, present := putBody["schedule_paused"]; present {
		t.Fatalf("schedule_paused must NOT be in the update PUT payload")
	}
	if !pausedCalled {
		t.Fatalf("pause endpoint was not called")
	}
}

// Defaults are applied for trigger_type/output_key/timezone in the update payload.
func TestUpdatePlaybook_Defaults(t *testing.T) {
	var putBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			_ = json.NewDecoder(r.Body).Decode(&putBody)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	_ = New(srv.URL, "tok", "").UpdatePlaybook(context.Background(), &Playbook{ID: 1, Name: "x"})
	for k, want := range map[string]string{"trigger_type": "manual", "output_key": "summary", "schedule_timezone": "UTC"} {
		if putBody[k] != want {
			t.Fatalf("%s = %v, want %q", k, putBody[k], want)
		}
	}
}
