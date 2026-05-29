package telemetry

import "testing"

func TestScrub(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "empty",
			input: "",
			want:  "",
		},
		{
			name:  "no PII",
			input: "authorization timed out",
			want:  "authorization timed out",
		},
		{
			name:  "token-shaped string",
			input: "invalid token abc1234567890abcdef1234",
			want:  "invalid token [redacted]",
		},
		{
			name:  "email",
			input: "user robin@us2.nl not found",
			want:  "user [redacted] not found",
		},
		{
			name:  "URL with query params",
			input: "request to https://studio.taufinity.io/api/auth?token=secret123 failed",
			want:  "request to https://studio.taufinity.io/api/auth failed",
		},
		{
			name:  "access token in message",
			input: "token eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9 expired",
			want:  "token [redacted] expired",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrub(tc.input)
			if got != tc.want {
				t.Errorf("scrub(%q)\n  got  %q\n  want %q", tc.input, got, tc.want)
			}
		})
	}
}
