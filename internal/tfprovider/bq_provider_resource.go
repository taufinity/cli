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
			"name":             schema.StringAttribute{Optional: true, Computed: true},
			"description":      schema.StringAttribute{Optional: true, Computed: true},
			"endpoint_url":     schema.StringAttribute{Optional: true, Computed: true, Description: "project.dataset"},
			"category":         schema.StringAttribute{Optional: true, Computed: true, Description: "Defaults to data_enrichment."},
			"http_method":      schema.StringAttribute{Optional: true, Computed: true, Description: "Defaults to GET."},
			"max_bytes_billed": schema.Int64Attribute{Optional: true, Computed: true},
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
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	Description    types.String `tfsdk:"description"`
	EndpointURL    types.String `tfsdk:"endpoint_url"`
	Category       types.String `tfsdk:"category"`
	HTTPMethod     types.String `tfsdk:"http_method"`
	MaxBytesBilled types.Int64  `tfsdk:"max_bytes_billed"`
	AllowedTables  types.List   `tfsdk:"allowed_tables"`
	Enabled        types.Bool   `tfsdk:"enabled"`
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
		ID:             types.StringValue(strconv.FormatInt(p.ID, 10)),
		Name:           types.StringValue(p.Name),
		Description:    types.StringValue(p.Description),
		EndpointURL:    types.StringValue(p.EndpointURL),
		Category:       types.StringValue(p.Category),
		HTTPMethod:     types.StringValue(p.HTTPMethod),
		MaxBytesBilled: types.Int64Value(p.MaxBytesBilled),
		AllowedTables:  tables,
		Enabled:        types.BoolValue(p.Enabled),
	}, nil
}

// fromPlan builds a studioadmin.BQProvider from a plan model, applying defaults for
// the create-required fields.
func fromPlan(ctx context.Context, m bqProviderModel) *studioadmin.BQProvider {
	var tables []string
	_ = m.AllowedTables.ElementsAs(ctx, &tables, false)
	category := m.Category.ValueString()
	if category == "" {
		category = "data_enrichment"
	}
	method := m.HTTPMethod.ValueString()
	if method == "" {
		method = "GET"
	}
	return &studioadmin.BQProvider{
		Name:           m.Name.ValueString(),
		Description:    m.Description.ValueString(),
		Category:       category,
		EndpointURL:    m.EndpointURL.ValueString(),
		HTTPMethod:     method,
		MaxBytesBilled: m.MaxBytesBilled.ValueInt64(),
		AllowedTables:  tables,
	}
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
	p := fromPlan(ctx, plan)
	p.ID = id
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

func (r *bqProviderResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan bqProviderModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	p := fromPlan(ctx, plan)
	id, err := r.client.CreateBQProvider(p)
	if err != nil {
		resp.Diagnostics.AddError("Create failed", err.Error())
		return
	}
	fresh, err := r.client.GetBQProvider(id)
	if err != nil {
		resp.Diagnostics.AddError("Read-after-create failed", err.Error())
		return
	}
	newState, err := toState(ctx, fresh)
	if err != nil {
		resp.Diagnostics.AddError("State conversion failed", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *bqProviderResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
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
	if err := r.client.DeleteBQProvider(id); err != nil {
		resp.Diagnostics.AddError("Delete failed", err.Error())
		return
	}
}

func (r *bqProviderResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
