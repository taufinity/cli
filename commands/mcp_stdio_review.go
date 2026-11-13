package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// localReviewToolName is the one tool the bridge answers itself. The review
// reads the local repository, which a remote MCP server cannot see, so the
// call never goes upstream; the bridge sends the review to Studio exactly as
// `taufinity review` does.
const localReviewToolName = "review_local_changes"

var localReviewTool = map[string]any{
	"name": localReviewToolName,
	"description": "Review the local git changes of the current repository before they are pushed to a branch that deploys. " +
		"Collects the working tree changes (or the staged ones, or whole files), drops secrets and files that must not leave the machine, " +
		"and returns the review as markdown with counts per severity, the verdict and the code review budget used. " +
		"Reads local git only, so it works with any git host.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"paths":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Limit the review to these files or folders (git pathspecs, relative to the working directory). Required with whole."},
			"whole":  map[string]any{"type": "boolean", "description": "Review the current content of the files under paths, changed or not."},
			"base":   map[string]any{"type": "string", "description": "Compare against this ref instead of the upstream, origin/main or main."},
			"staged": map[string]any{"type": "boolean", "description": "Review the staged changes instead of the working tree."},
		},
	},
}

// withLocalReviewTool appends the local tool to a tools/list result. A result
// that is not the expected shape, or that already lists the name, is returned
// as it came.
func withLocalReviewTool(result json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(result, &m); err != nil || m == nil {
		return result
	}
	var tools []json.RawMessage
	if raw, ok := m["tools"]; ok {
		if err := json.Unmarshal(raw, &tools); err != nil {
			return result
		}
	}
	for _, t := range tools {
		var named struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(t, &named) == nil && named.Name == localReviewToolName {
			return result
		}
	}
	tool, err := json.Marshal(localReviewTool)
	if err != nil {
		return result
	}
	m["tools"], err = json.Marshal(append(tools, tool))
	if err != nil {
		return result
	}
	out, err := json.Marshal(m)
	if err != nil {
		return result
	}
	return out
}

// handleLocalTool answers a tools/call for the local review tool and reports
// whether it did. Every other frame returns false and is forwarded unchanged.
func (b *bridge) handleLocalTool(ctx context.Context, frame jsonRPCFrame) bool {
	if frame.Method != "tools/call" {
		return false
	}
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(frame.Params, &call); err != nil || call.Name != localReviewToolName {
		return false
	}
	var id any
	if err := json.Unmarshal(frame.ID, &id); err != nil {
		return false
	}

	text, structured, isErr := runLocalReview(ctx, call.Arguments)
	result := map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isErr,
	}
	if structured != nil {
		result["structuredContent"] = structured
	}
	if err := b.writeFrame(map[string]any{"jsonrpc": "2.0", "id": id, "result": result}); err != nil {
		b.logf("write response: %v", err)
	}
	return true
}

// runLocalReview runs the review in-process for the tool's arguments and
// returns the text for the model, a structured summary, and whether it failed.
// It never writes to stdout, which is the protocol channel.
func runLocalReview(ctx context.Context, rawArgs json.RawMessage) (string, map[string]any, bool) {
	var args struct {
		Paths  []string `json:"paths"`
		Whole  bool     `json:"whole"`
		Base   string   `json:"base"`
		Staged bool     `json:"staged"`
	}
	if len(rawArgs) > 0 && string(rawArgs) != "null" {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return "Invalid arguments: paths is a list of strings, whole and staged are booleans, base is a string.", nil, true
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return err.Error(), nil, true
	}
	run := reviewRun{
		params: reviewParams{Paths: args.Paths, Whole: args.Whole, Staged: args.Staged, Base: args.Base},
		dir:    dir,
		client: newReviewClient(),
		log:    io.Discard,
		quiet:  true,
		dryRun: IsDryRun(),
	}
	out, err := run.execute(ctx)
	if err != nil {
		var refusal *studioRefusal
		if errors.As(err, &refusal) {
			msg := refusal.Message
			if h := refusal.Hint(); h != "" {
				msg += "\nHint: " + h
			}
			return msg, nil, true
		}
		return err.Error(), nil, true
	}

	var notes []string
	for _, s := range out.Payload.Skipped {
		notes = append(notes, fmt.Sprintf("Not sent: %s (%s)", s.Path, s.Reason))
	}
	if out.Payload.Masked > 0 {
		notes = append(notes, fmt.Sprintf("Masked %d secret value(s) before sending.", out.Payload.Masked))
	}
	switch {
	case out.NoChanges:
		return strings.Join(append([]string{"No changes to review."}, notes...), "\n"), map[string]any{"changes": false}, false
	case out.DryRun:
		return dryRunText(out.Payload, GetAPIURL()), map[string]any{"dry_run": true}, false
	}

	t := out.Terminal
	if t.Type == "error" {
		return fmt.Sprintf("The review failed (%s): %s", t.Code, t.Message), nil, true
	}
	parts := []string{strings.TrimRight(t.Markdown, "\n"), reviewSummary(t)}
	if out.Budget != "" {
		parts = append(parts, out.Budget)
	}
	parts = append(parts, notes...)
	structured := map[string]any{
		"verdict": t.Verdict, "counts": t.Counts, "partial": t.Partial,
		"masked": out.Payload.Masked,
	}
	if out.Budget != "" {
		structured["budget"] = out.Budget
	}
	return strings.Join(parts, "\n\n"), structured, false
}
