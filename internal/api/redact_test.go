package api

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRedactBodyForLog_RedactsSensitiveFields(t *testing.T) {
	cases := []struct {
		name   string
		body   map[string]any
		wantIn []string // substrings that must appear in the output
		wantNo []string // substrings that must NOT appear (the actual secret values)
	}{
		{
			name:   "totp_code",
			body:   map[string]any{"totp_code": "123456", "ttl_minutes": 960},
			wantIn: []string{`"totp_code":"[REDACTED]"`, `"ttl_minutes":960`},
			wantNo: []string{"123456"},
		},
		{
			name:   "backup_code",
			body:   map[string]any{"backup_code": "aaaaaaaa-bbbbbbbb"},
			wantIn: []string{`"backup_code":"[REDACTED]"`},
			wantNo: []string{"aaaaaaaa-bbbbbbbb"},
		},
		{
			name:   "password",
			body:   map[string]any{"password": "hunter2", "email": "a@b.com"},
			wantIn: []string{`"password":"[REDACTED]"`, `"email":"a@b.com"`},
			wantNo: []string{"hunter2"},
		},
		{
			name:   "no sensitive fields — unchanged",
			body:   map[string]any{"organization_id": 12, "name": "test"},
			wantIn: []string{`"organization_id":12`, `"name":"test"`},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, err := json.Marshal(c.body)
			if err != nil {
				t.Fatalf("marshal input: %v", err)
			}
			got := string(redactBodyForLog(raw))
			for _, want := range c.wantIn {
				if !strings.Contains(got, want) {
					t.Errorf("expected output to contain %q, got: %s", want, got)
				}
			}
			for _, no := range c.wantNo {
				if strings.Contains(got, no) {
					t.Errorf("expected output NOT to contain secret %q, got: %s", no, got)
				}
			}
		})
	}
}

func TestRedactBodyForLog_NonJSONBody_ReturnedUnchanged(t *testing.T) {
	raw := []byte("not json at all")
	got := redactBodyForLog(raw)
	if string(got) != string(raw) {
		t.Errorf("expected non-JSON body to pass through unchanged, got: %s", got)
	}
}

func TestRedactBodyForLog_EmptyBody(t *testing.T) {
	got := redactBodyForLog([]byte{})
	if len(got) != 0 {
		t.Errorf("expected empty body to stay empty, got: %s", got)
	}
}
