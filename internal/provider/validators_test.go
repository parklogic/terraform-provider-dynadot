package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestHostnameValidator(t *testing.T) {
	for _, tc := range []struct {
		value string
		valid bool
	}{{"ns1.example.com", true}, {"NS1.Example.COM.", true}, {"localhost", false}, {"-ns.example.com", false}, {"ns..com", false}, {"ns.example.com/", false}, {" ns.example.com", false}} {
		resp := validator.StringResponse{}
		hostnameValidator{}.ValidateString(context.Background(), validator.StringRequest{Path: path.Root("name"), ConfigValue: types.StringValue(tc.value)}, &resp)
		if resp.Diagnostics.HasError() == tc.valid {
			t.Errorf("%q: %v", tc.value, resp.Diagnostics)
		}
	}
}
func TestIPValidator(t *testing.T) {
	for _, tc := range []struct {
		name    string
		values  []attr.Value
		invalid bool
	}{
		{name: "IPv4", values: []attr.Value{types.StringValue("192.0.2.1")}},
		{name: "IPv6", values: []attr.Value{types.StringValue("2001:0db8::1")}},
		{name: "empty", invalid: true},
		{name: "null", values: []attr.Value{types.StringNull()}, invalid: true},
		{name: "unknown deferred", values: []attr.Value{types.StringUnknown()}},
		{name: "invalid", values: []attr.Value{types.StringValue("not an ip")}, invalid: true},
		{name: "zone", values: []attr.Value{types.StringValue("fe80::1%eth0")}, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := validator.SetResponse{}
			ipSetValidator{}.ValidateSet(context.Background(), validator.SetRequest{Path: path.Root("ip_addresses"), ConfigValue: types.SetValueMust(types.StringType, tc.values)}, &resp)
			if resp.Diagnostics.HasError() != tc.invalid {
				t.Fatal(resp.Diagnostics)
			}
		})
	}
}
func TestDurationValidator(t *testing.T) {
	for _, value := range []string{"5m", "1ns", "0s", "-1m", "invalid"} {
		resp := validator.StringResponse{}
		durationValidator{}.ValidateString(context.Background(), validator.StringRequest{Path: path.Root("timeout"), ConfigValue: types.StringValue(value)}, &resp)
		valid := value == "5m" || value == "1ns"
		if resp.Diagnostics.HasError() == valid {
			t.Errorf("%s: %v", value, resp.Diagnostics)
		}
	}
}
