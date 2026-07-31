// Package tfprovider is the Terraform provider frontend over pkg/studioadmin.
// It calls the Studio admin API directly through the shared client — it never
// shells out to the taufinity CLI binary.
package tfprovider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/taufinity/cli/pkg/studioadmin"
)

type taufinityProvider struct{}

// New returns the provider factory for providerserver.Serve.
func New() provider.Provider { return &taufinityProvider{} }

func (p *taufinityProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "taufinity"
}

func (p *taufinityProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manage Taufinity Studio configuration.",
		Attributes: map[string]schema.Attribute{
			"api_url": schema.StringAttribute{
				Optional:    true,
				Description: "Studio base URL. Defaults to $TAUFINITY_API_URL or https://studio.taufinity.io.",
			},
			"admin_token": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "Studio admin token (X-API-Key). Defaults to $TAUFINITY_ADMIN_TOKEN.",
			},
			"org": schema.StringAttribute{
				Optional:    true,
				Description: "Organization slug for org-scoped resources. Defaults to $TAUFINITY_ORG.",
			},
		},
	}
}

type providerConfig struct {
	APIURL     types.String `tfsdk:"api_url"`
	AdminToken types.String `tfsdk:"admin_token"`
	Org        types.String `tfsdk:"org"`
}

func (p *taufinityProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerConfig
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	apiURL := cfg.APIURL.ValueString()
	if apiURL == "" {
		apiURL = os.Getenv("TAUFINITY_API_URL")
	}
	if apiURL == "" {
		apiURL = "https://studio.taufinity.io"
	}
	token := cfg.AdminToken.ValueString()
	if token == "" {
		token = os.Getenv("TAUFINITY_ADMIN_TOKEN")
	}
	if token == "" {
		resp.Diagnostics.AddError(
			"Missing admin token",
			"Set admin_token in the provider block or the TAUFINITY_ADMIN_TOKEN environment variable.",
		)
		return
	}
	org := cfg.Org.ValueString()
	if org == "" {
		org = os.Getenv("TAUFINITY_ORG")
	}

	client := studioadmin.New(apiURL, token, org)
	resp.ResourceData = client
	resp.DataSourceData = client
}

func (p *taufinityProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewBQProviderResource,
		NewPlaybookResource,
	}
}

func (p *taufinityProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return nil
}
