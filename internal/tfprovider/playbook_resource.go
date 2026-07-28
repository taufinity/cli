package tfprovider

import (
	"context"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/taufinity/cli/pkg/studioadmin"
)

type playbookResource struct {
	client *studioadmin.Client
}

// NewPlaybookResource is the resource factory.
func NewPlaybookResource() resource.Resource { return &playbookResource{} }

func (r *playbookResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_playbook"
}

func (r *playbookResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A Taufinity Studio playbook's top-level config (schedule, enable, pause, agent). " +
			"Steps are not managed by this resource yet.",
		Attributes: map[string]schema.Attribute{
			"id":                 schema.StringAttribute{Computed: true, Description: "Studio numeric playbook id. Set on import."},
			"name":               schema.StringAttribute{Required: true},
			"description":        schema.StringAttribute{Optional: true, Computed: true},
			"trigger_type":       schema.StringAttribute{Optional: true, Computed: true, Description: "Defaults to manual."},
			"output_key":         schema.StringAttribute{Optional: true, Computed: true, Description: "Defaults to summary."},
			"schedule":           schema.StringAttribute{Optional: true, Computed: true, Description: "Cron; null when unscheduled."},
			"schedule_timezone":  schema.StringAttribute{Optional: true, Computed: true, Description: "Defaults to UTC."},
			"schedule_paused":    schema.BoolAttribute{Optional: true, Computed: true, Description: "Pause the cron without removing it."},
			"enabled":            schema.BoolAttribute{Optional: true, Computed: true},
			"agent_triggerable":  schema.BoolAttribute{Optional: true, Computed: true},
			"agent_input_schema": schema.StringAttribute{Optional: true, Computed: true},
		},
	}
}

type playbookModel struct {
	ID               types.String `tfsdk:"id"`
	Name             types.String `tfsdk:"name"`
	Description      types.String `tfsdk:"description"`
	TriggerType      types.String `tfsdk:"trigger_type"`
	OutputKey        types.String `tfsdk:"output_key"`
	Schedule         types.String `tfsdk:"schedule"`
	ScheduleTimezone types.String `tfsdk:"schedule_timezone"`
	SchedulePaused   types.Bool   `tfsdk:"schedule_paused"`
	Enabled          types.Bool   `tfsdk:"enabled"`
	AgentTriggerable types.Bool   `tfsdk:"agent_triggerable"`
	AgentInputSchema types.String `tfsdk:"agent_input_schema"`
}

func (r *playbookResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*studioadmin.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", "expected *studioadmin.Client")
		return
	}
	r.client = c
}

func playbookToState(p *studioadmin.Playbook) playbookModel {
	sched := types.StringNull()
	if p.Schedule != nil {
		sched = types.StringValue(*p.Schedule)
	}
	return playbookModel{
		ID:               types.StringValue(strconv.FormatInt(p.ID, 10)),
		Name:             types.StringValue(p.Name),
		Description:      types.StringValue(p.Description),
		TriggerType:      types.StringValue(p.TriggerType),
		OutputKey:        types.StringValue(p.OutputKey),
		Schedule:         sched,
		ScheduleTimezone: types.StringValue(p.ScheduleTimezone),
		SchedulePaused:   types.BoolValue(p.SchedulePaused),
		Enabled:          types.BoolValue(p.Enabled),
		AgentTriggerable: types.BoolValue(p.AgentTriggerable),
		AgentInputSchema: types.StringValue(p.AgentInputSchema),
	}
}

func playbookFromPlan(m playbookModel) *studioadmin.Playbook {
	var sched *string
	if !m.Schedule.IsNull() && !m.Schedule.IsUnknown() {
		v := m.Schedule.ValueString()
		sched = &v
	}
	return &studioadmin.Playbook{
		Name:             m.Name.ValueString(),
		Description:      m.Description.ValueString(),
		TriggerType:      m.TriggerType.ValueString(),
		OutputKey:        m.OutputKey.ValueString(),
		Schedule:         sched,
		ScheduleTimezone: m.ScheduleTimezone.ValueString(),
		SchedulePaused:   m.SchedulePaused.ValueBool(),
		Enabled:          m.Enabled.ValueBool(),
		AgentTriggerable: m.AgentTriggerable.ValueBool(),
		AgentInputSchema: m.AgentInputSchema.ValueString(),
	}
}

func (r *playbookResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state playbookModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := strconv.ParseInt(state.ID.ValueString(), 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Invalid id", err.Error())
		return
	}
	p, err := r.client.GetPlaybook(ctx, id)
	if err != nil {
		if studioadmin.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Read failed", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, playbookToState(p))...)
}

func (r *playbookResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan playbookModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p := playbookFromPlan(plan)
	id, err := r.client.CreatePlaybook(ctx, p)
	if err != nil {
		resp.Diagnostics.AddError("Create failed", err.Error())
		return
	}
	fresh, err := r.client.GetPlaybook(ctx, id)
	if err != nil {
		resp.Diagnostics.AddError("Read-after-create failed", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, playbookToState(fresh))...)
}

func (r *playbookResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state playbookModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := strconv.ParseInt(state.ID.ValueString(), 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Invalid id", err.Error())
		return
	}
	p := playbookFromPlan(plan)
	p.ID = id
	if err := r.client.UpdatePlaybook(ctx, p); err != nil {
		resp.Diagnostics.AddError("Update failed", err.Error())
		return
	}
	fresh, err := r.client.GetPlaybook(ctx, id)
	if err != nil {
		resp.Diagnostics.AddError("Read-after-update failed", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, playbookToState(fresh))...)
}

func (r *playbookResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state playbookModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := strconv.ParseInt(state.ID.ValueString(), 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Invalid id", err.Error())
		return
	}
	if err := r.client.DeletePlaybook(ctx, id); err != nil {
		resp.Diagnostics.AddError("Delete failed", err.Error())
		return
	}
}

func (r *playbookResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
