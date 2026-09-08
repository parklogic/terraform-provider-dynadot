package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/parklogic/terraform-provider-dynadot/internal/dynadot"
)

type nameserverRefreshAction struct{ client *dynadot.Client }
type refreshModel struct {
	Name    types.String `tfsdk:"name"`
	Timeout types.String `tfsdk:"timeout"`
}

var _ action.ActionWithConfigure = &nameserverRefreshAction{}

func NewNameserverRefreshAction() action.Action { return &nameserverRefreshAction{} }
func (*nameserverRefreshAction) Metadata(_ context.Context, req action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_nameserver_refresh"
}
func (*nameserverRefreshAction) Schema(_ context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = schema.Schema{Description: "Refreshes a registered nameserver by reading its current IPs and writing them back to Dynadot.", Attributes: map[string]schema.Attribute{
		"name":    schema.StringAttribute{Required: true, Description: "Fully qualified hostname of an existing registered nameserver.", Validators: []validator.String{hostnameValidator{}}},
		"timeout": schema.StringAttribute{Optional: true, Description: "Positive duration bounding the entire action, including rate-limit waits and retries. Defaults to 5m.", Validators: []validator.String{durationValidator{}}},
	}}
}
func (a *nameserverRefreshAction) Configure(_ context.Context, req action.ConfigureRequest, resp *action.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	var ok bool
	a.client, ok = req.ProviderData.(*dynadot.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected action configuration", fmt.Sprintf("Expected *dynadot.Client, got %T.", req.ProviderData))
	}
}
func (a *nameserverRefreshAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	var data refreshModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if data.Name.IsNull() || data.Name.IsUnknown() || !validHostname(data.Name.ValueString()) {
		resp.Diagnostics.AddError("Invalid hostname", "A known valid nameserver hostname is required.")
		return
	}
	if data.Timeout.IsUnknown() {
		resp.Diagnostics.AddError("Unknown timeout", "The timeout must be known before invocation.")
		return
	}
	timeout := 5 * time.Minute
	if !data.Timeout.IsNull() {
		var err error
		timeout, err = parseTimeout(data.Timeout.ValueString())
		if err != nil {
			resp.Diagnostics.AddError("Invalid timeout", err.Error())
			return
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	remote, err := a.client.GetNameserver(ctx, data.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Nameserver refresh failed", err.Error())
		return
	}
	if err = a.client.SetIPs(ctx, remote.Name, remote.IPAddresses); err != nil {
		resp.Diagnostics.AddError("Nameserver refresh failed", err.Error())
		return
	}
	if resp.SendProgress != nil {
		resp.SendProgress(action.InvokeProgressEvent{Message: "Nameserver IP addresses refreshed."})
	}
}
