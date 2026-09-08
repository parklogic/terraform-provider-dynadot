// Package provider implements the Dynadot Terraform provider.
package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/parklogic/terraform-provider-dynadot/internal/dynadot"
)

type dynadotProvider struct {
	version       string
	clientOptions []dynadot.Option
}
type providerModel struct {
	APIKey  types.String `tfsdk:"api_key"`
	Sandbox types.Bool   `tfsdk:"sandbox"`
}

var _ provider.ProviderWithActions = &dynadotProvider{}

func New(version string) func() provider.Provider {
	return func() provider.Provider { return &dynadotProvider{version: version} }
}
func (p *dynadotProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "dynadot"
	resp.Version = p.version
}
func (p *dynadotProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{Attributes: map[string]schema.Attribute{
		"api_key": schema.StringAttribute{Optional: true, Sensitive: true, Description: "Dynadot API key. Defaults to DYNADOT_API_KEY when omitted."},
		"sandbox": schema.BoolAttribute{Optional: true, Description: "Use the Dynadot sandbox API. Defaults to false."},
	}}
}
func (p *dynadotProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if config.APIKey.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("api_key"), "Unknown API key", "Set a known api_key or use DYNADOT_API_KEY.")
	}
	if config.Sandbox.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("sandbox"), "Unknown sandbox setting", "The sandbox setting must be known before configuration.")
	}
	if resp.Diagnostics.HasError() {
		return
	}
	key := config.APIKey.ValueString()
	if config.APIKey.IsNull() {
		key = os.Getenv("DYNADOT_API_KEY")
	}
	if key == "" {
		resp.Diagnostics.AddAttributeError(path.Root("api_key"), "Missing API key", "Set api_key or the DYNADOT_API_KEY environment variable.")
		return
	}
	client := dynadot.NewClient(key, config.Sandbox.ValueBool(), p.clientOptions...)
	_, recognized, err := client.Discover(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Account discovery failed", err.Error())
		return
	}
	if !recognized {
		resp.Diagnostics.AddWarning("Unknown account tier", "Dynadot returned a missing or unrecognized account tier. Conservative Regular account rate limits will be used.")
	}
	resp.ResourceData = client
	resp.ActionData = client
}
func (*dynadotProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{NewNameserverResource}
}
func (*dynadotProvider) DataSources(context.Context) []func() datasource.DataSource { return nil }
func (*dynadotProvider) Actions(context.Context) []func() action.Action {
	return []func() action.Action{NewNameserverRefreshAction}
}
