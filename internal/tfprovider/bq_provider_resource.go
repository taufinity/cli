package tfprovider

import (
	"context"
	"fmt"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/taufinity/cli/pkg/studioadmin"
)

type bqProviderResource struct {
	client *studioadmin.Client
}

// NewBQProviderResource is the resource factory.
func NewBQProviderResource() resource.Resource { return &bqProviderResource{} }

func (r *bqProviderResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_bq_provider"
}

func (r *bqProviderResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A Taufinity Studio BigQuery data provider. `allowed_tables` is the access boundary.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Studio numeric provider id. Set on import.",
			},
			"name":         schema.StringAttribute{Optional: true, Computed: true},
			"description":  schema.StringAttribute{Optional: true, Computed: true},
			"endpoint_url": schema.StringAttribute{Optional: true, Computed: true},
			"allowed_tables": schema.ListAttribute{
				ElementType: types.StringType,
				Required:    true,
				Description: "The exact tables this provider may read.",
			},
			"enabled": schema.BoolAttribute{Optional: true, Computed: true},
		},
	}
}

type bqProviderModel struct {
	ID            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	Description   types.String `tfsdk:"description"`
	EndpointURL   types.String `tfsdk:"endpoint_url"`
	AllowedTables types.List   `tfsdk:"allowed_tables"`
	Enabled       types.Bool   `tfsdk:"enabled"`
}

func (r *bqProviderResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// toState fills a model from a studioadmin.BQProvider.
func toState(ctx context.Context, p *studioadmin.BQProvider) (bqProviderModel, error) {
	tables, diags := types.ListValueFrom(ctx, types.StringType, p.AllowedTables)
	if diags.HasError() {
		return bqProviderModel{}, fmt.Errorf("convert allowed_tables")
	}
	return bqProviderModel{
		ID:            types.StringValue(strconv.FormatInt(p.ID, 10)),
		Name:          types.StringValue(p.Name),
		Description:   types.StringValue(p.Description),
		EndpointURL:   types.StringValue(p.EndpointURL),
		AllowedTables: tables,
		Enabled:       types.BoolValue(p.Enabled),
	}, nil
}

func (r *bqProviderResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state bqProviderModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := strconv.ParseInt(state.ID.ValueString(), 10, 64)
	if err != nil {
		resp.Diagnostics.AddError("Invalid id", err.Error())
		return
	}
	p, err := r.client.GetBQProvider(id)
	if err != nil {
		resp.Diagnostics.AddError("Read failed", err.Error())
		return
	}
	newState, err := toState(ctx, p)
	if err != nil {
		resp.Diagnostics.AddError("State conversion failed", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *bqProviderResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state bqProviderModel
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
	var tables []string
	resp.Diagnostics.Append(plan.AllowedTables.ElementsAs(ctx, &tables, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p := &studioadmin.BQProvider{
		ID:            id,
		Name:          plan.Name.ValueString(),
		Description:   plan.Description.ValueString(),
		EndpointURL:   plan.EndpointURL.ValueString(),
		AllowedTables: tables,
		Enabled:       plan.Enabled.ValueBool(),
	}
	if err := r.client.UpdateBQProvider(p); err != nil {
		resp.Diagnostics.AddError("Update failed", err.Error())
		return
	}
	// Read back to capture server-normalised state.
	fresh, err := r.client.GetBQProvider(id)
	if err != nil {
		resp.Diagnostics.AddError("Read-after-update failed", err.Error())
		return
	}
	newState, err := toState(ctx, fresh)
	if err != nil {
		resp.Diagnostics.AddError("State conversion failed", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

// Create/Delete of the composite (custom-ai-providers + admin/bq-providers) is a
// follow-up. This version manages existing providers: import, then apply changes to
// allowed_tables.
func (r *bqProviderResource) Create(_ context.Context, _ resource.CreateRequest, resp *resource.CreateResponse) {
	resp.Diagnostics.AddError(
		"Create not yet supported",
		"Import an existing provider first: terraform import taufinity_bq_provider.<name> <id>. Composite create is a follow-up.",
	)
}

func (r *bqProviderResource) Delete(_ context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	// Remove from Terraform state only; the Studio provider is left intact.
	resp.Diagnostics.AddWarning(
		"Provider left in Studio",
		"Removed from Terraform state but not deleted in Studio (composite delete is a follow-up). Delete via the CLI if intended.",
	)
}

func (r *bqProviderResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
