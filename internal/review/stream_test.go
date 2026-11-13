package review

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReadStream(t *testing.T) {
	t.Run("real_shape_result", func(t *testing.T) {
		term, err := ReadStream(bytes.NewReader(fixture(t, "stream_result.ndjson")), nil)
		if err != nil {
			t.Fatal(err)
		}
		if term.Type != "result" || term.Verdict != "request_changes" || term.Partial ||
			term.Counts["high"] != 1 || len(term.Findings) != 1 ||
			term.Findings[0] != (Finding{Severity: "high", Title: "Missing check", File: "main.go", Line: 2}) ||
			!strings.HasPrefix(term.Markdown, "## Summary\nOne problem.") {
			t.Fatalf("terminal: %+v", term)
		}
	})

	t.Run("real_shape_error", func(t *testing.T) {
		term, err := ReadStream(bytes.NewReader(fixture(t, "stream_error.ndjson")), nil)
		if err != nil {
			t.Fatal(err)
		}
		if term.Type != "error" || term.Code != "no_eu_model" || term.Message != "no EU model backend is configured" {
			t.Fatalf("terminal: %+v", term)
		}
	})

	t.Run("events_in_order", func(t *testing.T) {
		var got []string
		_, err := ReadStream(bytes.NewReader(fixture(t, "stream_result.ndjson")), func(e Event) {
			got = append(got, e.Type+":"+e.Stage+e.RequestID)
		})
		if err != nil {
			t.Fatal(err)
		}
		want := "accepted:3f1c2a9e-7b4d-4e8a-9c21-5d6e7f8a9b0c,progress:reviewing,keepalive:,progress:counter_investigation"
		if strings.Join(got, ",") != want {
			t.Fatalf("events %v", got)
		}
	})

	t.Run("unknown_type_ignored", func(t *testing.T) {
		in := `{"type":"future_thing","x":1}` + "\nnot json at all\n\n" + `{"a":1}` + "\n" +
			`{"markdown":"ok","type":"result","verdict":"approve"}` + "\n"
		term, err := ReadStream(strings.NewReader(in), func(e Event) { t.Errorf("unexpected event %+v", e) })
		if err != nil || term.Verdict != "approve" {
			t.Fatalf("term=%+v err=%v", term, err)
		}
	})

	t.Run("missing_terminal_is_error", func(t *testing.T) {
		in := `{"type":"accepted"}` + "\n" + `{"stage":"reviewing","type":"progress"}`
		if _, err := ReadStream(strings.NewReader(in), nil); err == nil {
			t.Fatal("a stream without a result or error must fail")
		}
	})

	t.Run("terminal_without_trailing_newline", func(t *testing.T) {
		term, err := ReadStream(strings.NewReader(`{"type":"error","code":"internal","message":"m"}`), nil)
		if err != nil || term.Code != "internal" {
			t.Fatalf("term=%+v err=%v", term, err)
		}
	})

	t.Run("line_over_4mib", func(t *testing.T) {
		md := strings.Repeat("x", 5<<20)
		line, _ := json.Marshal(map[string]any{"type": "result", "markdown": md, "verdict": "approve", "counts": map[string]int{}})
		in := `{"type":"accepted"}` + "\n" + string(line) + "\n"
		term, err := ReadStream(strings.NewReader(in), nil)
		if err != nil || len(term.Markdown) != len(md) {
			t.Fatalf("err=%v len=%d", err, len(term.Markdown))
		}
	})

	t.Run("raw_has_no_cost_usd", func(t *testing.T) {
		term, err := ReadStream(bytes.NewReader(fixture(t, "stream_result.ndjson")), nil)
		if err != nil {
			t.Fatal(err)
		}
		raw := string(term.Raw)
		if strings.Contains(raw, "cost_usd") || strings.Contains(raw, "0.4213") {
			t.Fatalf("raw carries the cost: %s", raw)
		}
		var back map[string]any
		if err := json.Unmarshal(term.Raw, &back); err != nil || back["verdict"] != "request_changes" {
			t.Fatalf("raw is not the terminal line: %v %v", err, back)
		}
	})

	t.Run("raw_strips_any_cost_or_usd_key", func(t *testing.T) {
		in := `{"type":"result","markdown":"m","verdict":"approve","cost_usd":1.5,"usage":{"total_cost":2,"price_usd":3,"tokens":9},"findings":[{"title":"t","cost_estimate":4}]}` + "\n"
		term, err := ReadStream(strings.NewReader(in), nil)
		if err != nil {
			t.Fatal(err)
		}
		raw := string(term.Raw)
		for _, bad := range []string{"cost", "usd", "1.5", "price"} {
			if strings.Contains(raw, bad) {
				t.Errorf("raw still has %q: %s", bad, raw)
			}
		}
		if !strings.Contains(raw, `"tokens":9`) || !strings.Contains(raw, `"verdict":"approve"`) {
			t.Errorf("raw lost other fields: %s", raw)
		}
	})
}
