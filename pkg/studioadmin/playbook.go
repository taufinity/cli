package studioadmin

import (
	"context"
	"fmt"
)

// Playbook is a Studio playbook's top-level configuration. Steps are child rows
// reconciled separately (POST/DELETE per step) and are NOT managed here yet — this
// type covers the lifecycle + schedule + enable/pause fields, which is the bulk of
// cross-customer management (roll out a schedule, pause everywhere, toggle enabled).
//
// `schedule_paused` is deliberately its own field: the update PUT ignores it (pausing
// has scheduling side effects), so it is applied via a dedicated endpoint.
// Optional fields are pointers: nil means "not set by the user", so the write path
// omits them and the API keeps its own default (avoids clobbering a field the user
// didn't specify — e.g. omitting `enabled` must not disable the playbook). Read
// always populates them from the API response.
type Playbook struct {
	ID               int64
	Name             string
	Description      *string
	TriggerType      *string
	OutputKey        *string
	ScheduleTimezone *string
	AgentInputSchema *string
	Schedule         *string // nullable cron
	SchedulePaused   *bool
	Enabled          *bool
	AgentTriggerable *bool
}

type playbookAPI struct {
	ID               int64   `json:"id"`
	Name             string  `json:"name"`
	Description      string  `json:"description"`
	TriggerType      string  `json:"trigger_type"`
	OutputKey        string  `json:"output_key"`
	ScheduleTimezone string  `json:"schedule_timezone"`
	AgentInputSchema string  `json:"agent_input_schema"`
	Schedule         *string `json:"schedule"`
	SchedulePaused   bool    `json:"schedule_paused"`
	Enabled          bool    `json:"enabled"`
	AgentTriggerable bool    `json:"agent_triggerable"`
}

func (a *playbookAPI) toPlaybook() *Playbook {
	// Read always populates every field (the API returns concrete values).
	return &Playbook{
		ID:               a.ID,
		Name:             a.Name,
		Description:      &a.Description,
		TriggerType:      &a.TriggerType,
		OutputKey:        &a.OutputKey,
		ScheduleTimezone: &a.ScheduleTimezone,
		AgentInputSchema: &a.AgentInputSchema,
		Schedule:         a.Schedule,
		SchedulePaused:   &a.SchedulePaused,
		Enabled:          &a.Enabled,
		AgentTriggerable: &a.AgentTriggerable,
	}
}

// GetPlaybook reads a playbook by id (GET /playbooks/{id}).
func (c *Client) GetPlaybook(ctx context.Context, id int64) (*Playbook, error) {
	var raw playbookAPI
	if err := c.Get(ctx, fmt.Sprintf("/playbooks/%d", id), &raw); err != nil {
		return nil, err
	}
	return raw.toPlaybook(), nil
}

// updatePayload is the PUT /playbooks/{id} body. Only user-set fields are included
// (nil pointer = omitted, so the API keeps its default). schedule_paused is excluded
// entirely — it has its own endpoint.
func (p *Playbook) updatePayload() map[string]any {
	out := map[string]any{"name": p.Name}
	putStr(out, "description", p.Description)
	putStr(out, "trigger_type", p.TriggerType)
	putStr(out, "output_key", p.OutputKey)
	putStr(out, "schedule_timezone", p.ScheduleTimezone)
	putStr(out, "schedule", p.Schedule)
	putStr(out, "agent_input_schema", p.AgentInputSchema)
	if p.Enabled != nil {
		out["enabled"] = *p.Enabled
	}
	if p.AgentTriggerable != nil {
		out["agent_triggerable"] = *p.AgentTriggerable
	}
	return out
}

func putStr(m map[string]any, key string, v *string) {
	if v != nil {
		m[key] = *v
	}
}

// CreatePlaybook creates a playbook (POST /playbooks/) and applies schedule_paused.
// Steps are not created here (follow-up). Returns the new id.
func (c *Client) CreatePlaybook(ctx context.Context, p *Playbook) (int64, error) {
	create := map[string]any{"name": p.Name}
	putStr(create, "description", p.Description)
	putStr(create, "trigger_type", p.TriggerType)
	putStr(create, "output_key", p.OutputKey)
	putStr(create, "schedule", p.Schedule)
	putStr(create, "agent_input_schema", p.AgentInputSchema)
	var created struct {
		ID int64 `json:"id"`
	}
	if err := c.Write(ctx, "POST", "/playbooks/", create, &created, nil); err != nil {
		return 0, err
	}
	if created.ID == 0 {
		return 0, fmt.Errorf("studioadmin: create playbook returned no id")
	}
	p.ID = created.ID
	// enabled/agent_triggerable/timezone are set via the update PUT.
	if err := c.UpdatePlaybook(ctx, p); err != nil {
		if delErr := c.DeletePlaybook(ctx, created.ID); delErr != nil {
			return 0, fmt.Errorf("studioadmin: created playbook id=%d, follow-up update failed (%v), rollback delete failed: %w", created.ID, err, delErr)
		}
		return 0, fmt.Errorf("studioadmin: create playbook rolled back — update failed: %w", err)
	}
	return created.ID, nil
}

// UpdatePlaybook writes the top-level fields (PUT /playbooks/{id}) then applies
// schedule_paused via the dedicated endpoint.
func (c *Client) UpdatePlaybook(ctx context.Context, p *Playbook) error {
	if err := c.Write(ctx, "PUT", fmt.Sprintf("/playbooks/%d", p.ID), p.updatePayload(), nil, nil); err != nil {
		return err
	}
	// Only touch the pause state when the user set it (nil = leave as-is).
	if p.SchedulePaused != nil {
		return c.SetPlaybookPaused(ctx, p.ID, *p.SchedulePaused)
	}
	return nil
}

// SetPlaybookPaused pauses/resumes the schedule (POST /playbooks/{id}/schedule/pause).
func (c *Client) SetPlaybookPaused(ctx context.Context, id int64, paused bool) error {
	return c.Write(ctx, "POST", fmt.Sprintf("/playbooks/%d/schedule/pause", id),
		map[string]bool{"paused": paused}, nil, nil)
}

// DeletePlaybook deletes a playbook (DELETE /playbooks/{id}).
func (c *Client) DeletePlaybook(ctx context.Context, id int64) error {
	return c.Write(ctx, "DELETE", fmt.Sprintf("/playbooks/%d", id), nil, nil, nil)
}
