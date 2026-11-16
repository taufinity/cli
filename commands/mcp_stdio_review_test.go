package commands

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// bridgeCall sends one frame and returns the decoded response line.
func bridgeCall(t *testing.T, in interface{ Write([]byte) (int, error) }, out *threadSafeBuffer, frame string) (map[string]any, string) {
	t.Helper()
	if _, err := in.Write([]byte(frame + "\n")); err != nil {
		t.Fatal(err)
	}
	line := readNextFrame(t, out, 20*time.Second)
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("bad frame %q: %v", line, err)
	}
	return m, string(line)
}

func TestMCPReviewLocalChanges(t *testing.T) {
	startBridge := func(t *testing.T, studio *fakeStudio) (*mockUpstream, interface{ Write([]byte) (int, error) }, *threadSafeBuffer) {
		t.Helper()
		resetGlobals(t)
		useAPIKey(t)
		dir := newReviewTestRepo(t)
		wd, _ := os.Getwd()
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(wd) })
		flagAPIURL = studio.srv.URL
		mu := newMockUpstream()
		t.Cleanup(mu.close)
		mu.responseBody.Store(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"foo"}]}}`)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		t.Cleanup(cancel)
		in, out, _, _ := runBridgeAsync(t, ctx, mu.url())
		return mu, in, out.(*threadSafeBuffer)
	}
	studio := func(t *testing.T) *fakeStudio { return newFakeStudio(t, readFixture(t, "stream_result.ndjson")) }

	t.Run("listed_in_tools_list", func(t *testing.T) {
		_, in, out := startBridge(t, studio(t))
		m, _ := bridgeCall(t, in, out, `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)
		var names []string
		for _, tool := range m["result"].(map[string]any)["tools"].([]any) {
			names = append(names, tool.(map[string]any)["name"].(string))
		}
		if strings.Join(names, ",") != "foo,review_local_changes" {
			t.Fatalf("tools %v", names)
		}
	})

	t.Run("call_runs_review_locally", func(t *testing.T) {
		f := studio(t)
		mu, in, out := startBridge(t, f)
		m, _ := bridgeCall(t, in, out, `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"review_local_changes","arguments":{"paths":["main.go"]}}}`)
		if m["id"].(float64) != 7 {
			t.Fatalf("id %v", m["id"])
		}
		result := m["result"].(map[string]any)
		text := result["content"].([]any)[0].(map[string]any)["text"].(string)
		for _, want := range []string{"## Summary\nOne problem.", "1 high", "verdict: request_changes", "Code review budget used: 6% today, 2% this month"} {
			if !strings.Contains(text, want) {
				t.Errorf("text lacks %q:\n%s", want, text)
			}
		}
		if result["isError"] != false {
			t.Errorf("isError %v", result["isError"])
		}
		if len(f.posts()) != 1 {
			t.Errorf("Studio saw %d reviews", len(f.posts()))
		}
		mu.mu.Lock()
		defer mu.mu.Unlock()
		for _, method := range mu.methods {
			if method == "tools/call" {
				t.Error("the review call went upstream")
			}
		}
	})

	t.Run("call_reports_a_refusal", func(t *testing.T) {
		f := newFakeStudio(t, `{"error":"not_enabled","message":"Code review is not switched on."}`)
		f.reviewStatus = 403
		_, in, out := startBridge(t, f)
		m, _ := bridgeCall(t, in, out, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"review_local_changes","arguments":{}}}`)
		result := m["result"].(map[string]any)
		text := result["content"].([]any)[0].(map[string]any)["text"].(string)
		if result["isError"] != true || !strings.Contains(text, "not switched on") || !strings.Contains(text, "switch on code review") {
			t.Errorf("result %v", result)
		}
	})

	t.Run("result_has_no_usd", func(t *testing.T) {
		_, in, out := startBridge(t, studio(t))
		_, raw := bridgeCall(t, in, out, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"review_local_changes","arguments":{}}}`)
		for _, bad := range []string{"0.42", "0.4213", "$", "USD", "usd", "cost_usd"} {
			if strings.Contains(raw, bad) {
				t.Errorf("the frame contains %q:\n%s", bad, raw)
			}
		}
	})

	t.Run("other_calls_pass_through", func(t *testing.T) {
		f := studio(t)
		mu, in, out := startBridge(t, f)
		mu.responseBody.Store("") // default: echo the id with {"ok":true}
		m, _ := bridgeCall(t, in, out, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"foo","arguments":{}}}`)
		if res, _ := m["result"].(map[string]any); res["ok"] != true {
			t.Fatalf("response %v", m)
		}
		mu.mu.Lock()
		methods := append([]string(nil), mu.methods...)
		mu.mu.Unlock()
		if len(methods) != 1 || methods[0] != "tools/call" {
			t.Errorf("upstream methods %v", methods)
		}
		if len(f.requests()) != 0 {
			t.Error("a pass-through call must not touch the review endpoints")
		}
	})
}
