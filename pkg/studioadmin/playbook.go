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
type Playbook struct {
	ID               int64
	Name             string
	Description      string
	TriggerType      string
	OutputKey        string
	ScheduleTimezone string
	AgentInputSchema string
	Schedule         *string // nullable cron
	SchedulePaused   bool
	Enabled          bool
	AgentTriggerable bool
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
	return &Playbook{
		ID:               a.ID,
		Name:             a.Name,
		Description:      a.Description,
		TriggerType:      a.TriggerType,
		OutputKey:        a.OutputKey,
		ScheduleTimezone: a.ScheduleTimezone,
		AgentInputSchema: a.AgentInputSchema,
		Schedule:         a.Schedule,
		SchedulePaused:   a.SchedulePaused,
		Enabled:          a.Enabled,
		AgentTriggerable: a.AgentTriggerable,
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

// updatePayload is the PUT /playbooks/{id} body (schedule_paused excluded — applied
// via the pause endpoint).
func (p *Playbook) updatePayload() map[string]any {
	out := map[string]any{
		"name":              p.Name,
		"description":       p.Description,
		"trigger_type":      defaultStr(p.TriggerType, "manual"),
		"output_key":        defaultStr(p.OutputKey, "summary"),
		"schedule_timezone": defaultStr(p.ScheduleTimezone, "UTC"),
		"enabled":           p.Enabled,
		"agent_triggerable": p.AgentTriggerable,
	}
	if p.Schedule != nil {
		out["schedule"] = *p.Schedule
	}
	if p.AgentInputSchema != "" {
		out["agent_input_schema"] = p.AgentInputSchema
	}
	return out
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// CreatePlaybook creates a playbook (POST /playbooks/) and applies schedule_paused.
// Steps are not created here (follow-up). Returns the new id.
func (c *Client) CreatePlaybook(ctx context.Context, p *Playbook) (int64, error) {
	create := map[string]any{
		"name":         p.Name,
		"description":  p.Description,
		"trigger_type": defaultStr(p.TriggerType, "manual"),
		"output_key":   defaultStr(p.OutputKey, "summary"),
	}
	if p.Schedule != nil {
		create["schedule"] = *p.Schedule
	}
	if p.AgentInputSchema != "" {
		create["agent_input_schema"] = p.AgentInputSchema
	}
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
	return c.SetPlaybookPaused(ctx, p.ID, p.SchedulePaused)
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
