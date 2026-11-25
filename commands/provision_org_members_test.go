package commands

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
)

// orgMembersFakeStudio answers the three calls upsertOrgMembers makes, in the
// shapes Studio returns them: the member list, the pending invitations
// (status 200, or invitesStatus when set) and the add-member POST, which
// answers 202 {"status":"invitation_sent"} for a caller that is not a system
// admin.
type orgMembersFakeStudio struct {
	invitesStatus int
	mu            sync.Mutex
	posted        []string
}

func (s *orgMembersFakeStudio) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/organizations/12/members", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"user_id":1,"role":"admin","user":{"email":"Member@Example.com"}}]`))
	})
	mux.HandleFunc("GET /api/admin/invitations", func(w http.ResponseWriter, r *http.Request) {
		if s.invitesStatus != 0 {
			w.WriteHeader(s.invitesStatus)
			_, _ = w.Write([]byte(`{"error":"feature_disabled"}`))
			return
		}
		if r.Header.Get("X-Organization-ID") != "12" || r.URL.Query().Get("status") != "pending" {
			http.Error(w, "want org 12 and status=pending", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"invitations":[
			{"id":7,"email":"pending@example.com","status":"pending","target_type":"org_member"},
			{"id":8,"email":"portal@example.com","status":"pending","target_type":"client_group_member"}
		],"total":2}`))
	})
	mux.HandleFunc("POST /api/organizations/12/members", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]string
		_ = json.Unmarshal(b, &req)
		s.mu.Lock()
		s.posted = append(s.posted, req["email"])
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"invitation_sent"}`))
	})
	return mux
}

func TestUpsertOrgMembersInvitations(t *testing.T) {
	cfg := orgMembersConfig{Members: []orgMemberEntry{
		{Email: "member@example.com", Role: "admin"},
		{Email: "Pending@Example.com", Role: "member"},
		{Email: "portal@example.com", Role: "member"},
		{Email: "new@example.com", Role: "viewer"},
	}}

	run := func(t *testing.T, s *orgMembersFakeStudio) []string {
		t.Helper()
		ts := httptest.NewServer(s.handler())
		defer ts.Close()
		c := newProvisionClient(ts.URL, "test-key", false)
		if err := upsertOrgMembers(c, 12, cfg); err != nil {
			t.Fatalf("upsertOrgMembers: %v", err)
		}
		sort.Strings(s.posted)
		return s.posted
	}

	t.Run("invitation_sent_is_success_and_pending_invites_are_skipped", func(t *testing.T) {
		got := run(t, &orgMembersFakeStudio{})
		// member: already in; Pending: pending org_member invite (case-insensitive);
		// portal: its pending invite is for a client group, not the org.
		want := []string{"new@example.com", "portal@example.com"}
		if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("posted %v, want %v", got, want)
		}
	})

	t.Run("unreadable_invitation_list_invites_everyone_missing", func(t *testing.T) {
		got := run(t, &orgMembersFakeStudio{invitesStatus: http.StatusForbidden})
		if len(got) != 3 {
			t.Fatalf("posted %v, want the three non-members", got)
		}
	})
}
