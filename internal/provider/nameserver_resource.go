package provider

import (
	"context"
	"fmt"
	"slices"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/parklogic/terraform-provider-dynadot/internal/dynadot"
)

type nameserverResource struct{ client *dynadot.Client }
type nameserverModel struct {
	Name        types.String `tfsdk:"name"`
	IPAddresses types.Set    `tfsdk:"ip_addresses"`
}

var _ resource.ResourceWithConfigure = &nameserverResource{}
var _ resource.ResourceWithImportState = &nameserverResource{}

func NewNameserverResource() resource.Resource { return &nameserverResource{} }
func (*nameserverResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_nameserver"
}
func (*nameserverResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{Description: "Manages a registered Dynadot nameserver and its glue IP addresses.", Attributes: map[string]schema.Attribute{
		"name":         schema.StringAttribute{Required: true, Description: "Fully qualified nameserver hostname. Changing it replaces the registration.", Validators: []validator.String{hostnameValidator{}}, PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
		"ip_addresses": schema.SetAttribute{Required: true, ElementType: types.StringType, Description: "Nonempty set of IPv4 or IPv6 glue addresses.", Validators: []validator.Set{ipSetValidator{}}},
	}}
}
func (r *nameserverResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	var ok bool
	r.client, ok = req.ProviderData.(*dynadot.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected resource configuration", fmt.Sprintf("Expected *dynadot.Client, got %T.", req.ProviderData))
	}
}
func (m nameserverModel) ips(ctx context.Context, diags *diag.Diagnostics) []string {
	if m.Name.IsNull() || m.Name.IsUnknown() || !validHostname(m.Name.ValueString()) {
		diags.AddError("Invalid hostname", "A known, valid nameserver hostname is required.")
		return nil
	}
	if m.IPAddresses.IsNull() || m.IPAddresses.IsUnknown() {
		diags.AddError("Invalid IP addresses", "A known nonempty IP address set is required.")
		return nil
	}
	var ips []string
	for _, element := range m.IPAddresses.Elements() {
		value, ok := element.(types.String)
		if !ok || value.IsNull() || value.IsUnknown() {
			diags.AddError("Invalid IP addresses", "Every IP address must be known and non-null.")
			return nil
		}
		ips = append(ips, value.ValueString())
	}
	normalized, err := dynadot.NormalizeIPs(ips)
	if err != nil || len(normalized) == 0 {
		diags.AddError("Invalid IP addresses", "At least one valid IPv4 or IPv6 address is required.")
		return nil
	}
	return normalized
}

// Preserve each configured representation when the API returns an equivalent address.
func (m *nameserverModel) refresh(ctx context.Context, remote dynadot.Nameserver, diags *diag.Diagnostics) {
	if m.Name.IsNull() || m.Name.IsUnknown() || dynadot.NormalizeHostname(m.Name.ValueString()) != remote.Name {
		m.Name = types.StringValue(remote.Name)
	}
	represented := make([]string, 0, len(remote.IPAddresses))
	seen := map[string]bool{}
	for _, element := range m.IPAddresses.Elements() {
		value, ok := element.(types.String)
		if !ok || value.IsNull() || value.IsUnknown() {
			continue
		}
		ips, err := dynadot.NormalizeIPs([]string{value.ValueString()})
		if err == nil && slices.Contains(remote.IPAddresses, ips[0]) {
			represented = append(represented, value.ValueString())
			seen[ips[0]] = true
		}
	}
	for _, ip := range remote.IPAddresses {
		if !seen[ip] {
			represented = append(represented, ip)
		}
	}
	value, d := types.SetValueFrom(ctx, types.StringType, represented)
	diags.Append(d...)
	m.IPAddresses = value
}
func (r *nameserverResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var data nameserverModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ips := data.ips(ctx, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.RegisterNameserver(ctx, data.Name.ValueString(), ips[0]); err != nil {
		resp.Diagnostics.AddError("Register nameserver failed", err.Error())
		return
	}
	// Track the registration before any subsequent operation can fail.
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.SetIPs(ctx, data.Name.ValueString(), ips); err != nil {
		resp.Diagnostics.AddError("Set nameserver IPs failed", err.Error())
		return
	}
	remote, err := r.client.GetNameserver(ctx, data.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Read nameserver failed", err.Error())
		return
	}
	data.refresh(ctx, remote, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
func (r *nameserverResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var data nameserverModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	remote, err := r.client.GetNameserver(ctx, data.Name.ValueString())
	if dynadot.IsNotFound(err) {
		resp.State.RemoveResource(ctx)
		return
	}
	if err != nil {
		resp.Diagnostics.AddError("Read nameserver failed", err.Error())
		return
	}
	data.refresh(ctx, remote, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
func (r *nameserverResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var data nameserverModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ips := data.ips(ctx, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.SetIPs(ctx, data.Name.ValueString(), ips); err != nil {
		resp.Diagnostics.AddError("Set nameserver IPs failed", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
	remote, err := r.client.GetNameserver(ctx, data.Name.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Read nameserver failed", err.Error())
		return
	}
	data.refresh(ctx, remote, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
func (r *nameserverResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var data nameserverModel
	resp.Diagnostics.Append(req.State.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.client.DeleteNameserver(ctx, data.Name.ValueString()); err != nil && !dynadot.IsNotFound(err) {
		resp.Diagnostics.AddError("Delete nameserver failed", err.Error())
	}
}
func (*nameserverResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	if !validHostname(req.ID) {
		resp.Diagnostics.AddError("Invalid import hostname", "Import using a fully qualified nameserver hostname.")
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), req.ID)...)
}
