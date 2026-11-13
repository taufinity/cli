package review

import (
	"strings"
	"testing"
)

func TestMask(t *testing.T) {
	cases := []struct{ name, in, secret string }{
		{"aws", `id = AKIAABCDEFGHIJKLMNOP`, "AKIAABCDEFGHIJKLMNOP"},                                                                                                 // gitleaks:allow -- clearly fake test value
		{"github", `x ghp_abcdefghijklmnopqrstuvwxyz0123456789 y`, "ghp_abcdefghijklmnopqrstuvwxyz0123456789"},                                                       // gitleaks:allow -- clearly fake test value
		{"slack", `xoxb-123456789012-abcdefghijkl`, "xoxb-123456789012-abcdefghijkl"},                                                                                // gitleaks:allow -- clearly fake test value
		{"stripe", `key sk_live_abcdefghijklmnop1234`, "sk_live_abcdefghijklmnop1234"},                                                                               // gitleaks:allow -- clearly fake test value
		{"google", `k=AIzaSyA1234567890abcdefghijklmnopqrstuv`, "AIzaSyA1234567890abcdefghijklmnopqrstuv"},                                                           // gitleaks:allow -- clearly fake test value
		{"jwt", `t eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghij1234567890 `, "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghij1234567890"}, // gitleaks:allow -- clearly fake test value
		{"assignment", `db_password = "hunter2hunter2"`, "hunter2hunter2"},                                                                                           // gitleaks:allow -- clearly fake test value
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, n := Mask(c.in)
			if n != 1 || strings.Contains(got, c.secret) || !strings.Contains(got, "[MASKED]") {
				t.Fatalf("Mask(%q) = %q, %d", c.in, got, n)
			}
		})
	}

	t.Run("pem_block", func(t *testing.T) {
		in := "+-----BEGIN RSA PRIVATE KEY-----\n+MIIEowIBAAKCAQEAfakefakefakefake\n+abcdefabcdefabcdefabcdef\n+-----END RSA PRIVATE KEY-----\n+after\n" // gitleaks:allow -- clearly fake test value
		got, n := Mask(in)
		if n != 1 || strings.Contains(got, "MIIEow") || strings.Contains(got, "END RSA") || !strings.Contains(got, "+after") {
			t.Fatalf("Mask = %q, %d", got, n)
		}
	})

	t.Run("assignment_keeps_short_values", func(t *testing.T) {
		in := `password = "short"` // gitleaks:allow -- clearly fake test value
		if got, n := Mask(in); n != 0 || got != in {
			t.Fatalf("Mask = %q, %d", got, n)
		}
	})

	t.Run("leaves_normal_code", func(t *testing.T) {
		in := "func main() {\n\ttokenCount := 5\n\tname := \"a long ordinary string value\"\n}\n"
		if got, n := Mask(in); n != 0 || got != in {
			t.Fatalf("Mask changed normal code: %q, %d", got, n)
		}
	})
}

func TestMaskUnquotedAndURLCredentials(t *testing.T) {
	for _, in := range []string{
		"+DB_PASSWORD=hunter2hunter2",                   // gitleaks:allow -- clearly fake test value
		"+password: hunter2hunter2",                     // gitleaks:allow -- clearly fake test value
		"+export API_TOKEN=abcd1234efgh5678",            // gitleaks:allow -- clearly fake test value
		"+postgres://u:pw12345678@host/db",              // gitleaks:allow -- clearly fake test value
		"+Authorization: Bearer abcdef0123456789abcdef", // gitleaks:allow -- clearly fake test value
	} {
		out, n := Mask(in)
		if n == 0 || strings.Contains(out, "hunter2hunter2") || strings.Contains(out, "abcd1234efgh5678") ||
			strings.Contains(out, "pw12345678") || strings.Contains(out, "abcdef0123456789abcdef") {
			t.Errorf("not masked: %q -> %q (%d)", in, out, n)
		}
	}
	if out, n := Mask("token = f(x)\nsecret := load()"); n != 0 {
		t.Errorf("code masked: %q", out)
	}
}

func TestExcludedPathCaseInsensitiveEnv(t *testing.T) {
	for _, p := range []string{".ENV", "cfg/.Env.local", "ID_RSA"} {
		if !excludedPath(p) {
			t.Errorf("%s not excluded", p)
		}
	}
}
