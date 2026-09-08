package provider

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/parklogic/terraform-provider-dynadot/internal/dynadot"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func options(f roundTripFunc) []dynadot.Option {
	return []dynadot.Option{dynadot.WithHTTPClient(&http.Client{Transport: f})}
}
func resourceState(t *testing.T, m nameserverModel) tfsdk.State {
	t.Helper()
	var schema resource.SchemaResponse
	(&nameserverResource{}).Schema(context.Background(), resource.SchemaRequest{}, &schema)
	state := tfsdk.State{Schema: schema.Schema}
	if d := state.Set(context.Background(), &m); d.HasError() {
		t.Fatal(d)
	}
	return state
}
func model(ips ...string) nameserverModel {
	set, _ := types.SetValueFrom(context.Background(), types.StringType, ips)
	return nameserverModel{Name: types.StringValue("NS1.Example.COM."), IPAddresses: set}
}
func TestConfigure(t *testing.T) {
	for _, tc := range []struct {
		name, key, env, tier                               string
		unknownKey, unknownSandbox, wantError, wantWarning bool
	}{
		{name: "explicit", key: "explicit", env: "environment", tier: "Super Bulk"},
		{name: "environment", env: "environment", tier: "Bulk"},
		{name: "missing", wantError: true},
		{name: "unknown key", env: "environment", unknownKey: true, wantError: true},
		{name: "unknown sandbox", key: "key", unknownSandbox: true, wantError: true},
		{name: "missing tier", key: "key", wantWarning: true},
		{name: "unknown tier", key: "key", tier: "Future", wantWarning: true},
		{name: "discovery error", key: "key", tier: "error", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DYNADOT_API_KEY", tc.env)
			calls := 0
			p := &dynadotProvider{clientOptions: options(func(req *http.Request) (*http.Response, error) {
				calls++
				want := tc.key
				if want == "" {
					want = tc.env
				}
				if req.Header.Get("Authorization") != "Bearer "+want {
					t.Error("credential precedence")
				}
				if tc.tier == "error" {
					return response(`{"code":401}`), nil
				}
				return response(`{"code":200,"data":{"account_info":{"price_level":"` + tc.tier + `"}}}`), nil
			})}
			var s provider.SchemaResponse
			p.Schema(context.Background(), provider.SchemaRequest{}, &s)
			data := providerModel{APIKey: types.StringNull(), Sandbox: types.BoolNull()}
			if tc.key != "" {
				data.APIKey = types.StringValue(tc.key)
			}
			if tc.unknownKey {
				data.APIKey = types.StringUnknown()
			}
			if tc.unknownSandbox {
				data.Sandbox = types.BoolUnknown()
			}
			state := tfsdk.State{Schema: s.Schema}
			if d := state.Set(context.Background(), &data); d.HasError() {
				t.Fatal(d)
			}
			req := provider.ConfigureRequest{Config: tfsdk.Config{Schema: s.Schema, Raw: state.Raw}}
			var previous any
			for range 2 {
				var resp provider.ConfigureResponse
				p.Configure(context.Background(), req, &resp)
				if resp.Diagnostics.HasError() != tc.wantError {
					t.Fatal(resp.Diagnostics)
				}
				if (resp.Diagnostics.WarningsCount() > 0) != tc.wantWarning {
					t.Fatal(resp.Diagnostics)
				}
				if !tc.wantError {
					if resp.ResourceData == nil || resp.ResourceData != resp.ActionData {
						t.Fatal("client is not shared")
					}
					if previous == resp.ResourceData {
						t.Fatal("client reused across Configure")
					}
					previous = resp.ResourceData
				}
			}
			if !tc.wantError && calls != 2 {
				t.Fatalf("discovery calls = %d", calls)
			}
		})
	}
}
func TestNameserverCRUD(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var methods []string
		client := dynadot.NewClient("key", false, options(func(req *http.Request) (*http.Response, error) {
			methods = append(methods, req.Method)
			if req.Method == "GET" {
				return response(`{"code":200,"data":{"server_name":"ns1.example.com","ip_list":["2001:db8::1","192.0.2.1"]}}`), nil
			}
			body, _ := io.ReadAll(req.Body)
			if req.Method == "POST" && !strings.Contains(string(body), `"ip":"192.0.2.1"`) {
				t.Errorf("first IP not sorted: %s", body)
			}
			return response(`{"code":200}`), nil
		})...)
		r := &nameserverResource{client: client}
		desired := model("2001:0db8:0000:0000:0000:0000:0000:0001", "192.0.2.1")
		state := resourceState(t, desired)
		plan := tfsdk.Plan(state)
		create := resource.CreateResponse{State: tfsdk.State{Schema: state.Schema}}
		r.Create(context.Background(), resource.CreateRequest{Plan: plan}, &create)
		if create.Diagnostics.HasError() {
			t.Fatal(create.Diagnostics)
		}
		if !create.State.Raw.Equal(state.Raw) {
			t.Fatal("create changed equivalent configuration")
		}
		read := resource.ReadResponse{State: create.State}
		r.Read(context.Background(), resource.ReadRequest{State: create.State}, &read)
		if read.Diagnostics.HasError() || !read.State.Raw.Equal(state.Raw) {
			t.Fatal(read.Diagnostics, "read changed representation")
		}
		update := resource.UpdateResponse{State: read.State}
		r.Update(context.Background(), resource.UpdateRequest{Plan: plan, State: read.State}, &update)
		if update.Diagnostics.HasError() {
			t.Fatal(update.Diagnostics)
		}
		del := resource.DeleteResponse{State: update.State}
		r.Delete(context.Background(), resource.DeleteRequest{State: update.State}, &del)
		if del.Diagnostics.HasError() {
			t.Fatal(del.Diagnostics)
		}
		if strings.Join(methods, ",") != "POST,PUT,GET,GET,PUT,GET,DELETE" {
			t.Fatal(methods)
		}
	})
}
func TestPartialCreate(t *testing.T) {
	for _, failure := range []string{"POST", "PUT", "GET"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := &nameserverResource{client: dynadot.NewClient("key", false, options(func(req *http.Request) (*http.Response, error) {
					if req.Method == failure {
						return response(`{"code":400}`), nil
					}
					return response(`{"code":200}`), nil
				})...)}
				state := resourceState(t, model("192.0.2.1"))
				resp := resource.CreateResponse{State: tfsdk.State{Schema: state.Schema}}
				r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan(state)}, &resp)
				if !resp.Diagnostics.HasError() {
					t.Fatal("expected failure")
				}
				if failure != "POST" && (resp.State.Raw.IsNull() || !resp.State.Raw.Equal(state.Raw)) {
					t.Fatal("registration lost after partial failure")
				}
			})
		})
	}
}
func TestReadAndDeleteAbsenceConflict(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantError  bool
	}{
		{name: "not found code", body: `{"code":404}`},
		{name: "not found description", body: `{"code":401,"error":{"description":"Could not find this nameserver"}}`},
		{name: "conflict", body: `{"code":409}`, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := &nameserverResource{client: dynadot.NewClient("key", false, options(func(*http.Request) (*http.Response, error) { return response(tc.body), nil })...)}
				state := resourceState(t, model("192.0.2.1"))
				read := resource.ReadResponse{State: state}
				r.Read(context.Background(), resource.ReadRequest{State: state}, &read)
				if tc.wantError != read.Diagnostics.HasError() {
					t.Fatal(read.Diagnostics)
				}
				if !tc.wantError && !read.State.Raw.IsNull() {
					t.Fatal("missing resource retained")
				}
				del := resource.DeleteResponse{State: state}
				r.Delete(context.Background(), resource.DeleteRequest{State: state}, &del)
				if tc.wantError != del.Diagnostics.HasError() {
					t.Fatal(del.Diagnostics)
				}
				if tc.wantError && !del.State.Raw.Equal(state.Raw) {
					t.Fatal("conflict discarded state")
				}
			})
		})
	}
}
func TestImport(t *testing.T) {
	r := &nameserverResource{}
	state := resourceState(t, nameserverModel{Name: types.StringNull(), IPAddresses: types.SetNull(types.StringType)})
	for _, name := range []string{"NS1.Example.COM.", "bad/name"} {
		resp := resource.ImportStateResponse{State: state}
		r.ImportState(context.Background(), resource.ImportStateRequest{ID: name}, &resp)
		if resp.Diagnostics.HasError() != (name == "bad/name") {
			t.Fatal(resp.Diagnostics)
		}
		if name != "bad/name" {
			var got nameserverModel
			resp.State.Get(context.Background(), &got)
			if got.Name.ValueString() != name || !got.IPAddresses.IsNull() {
				t.Fatal(got)
			}
		}
	}
}
func actionConfig(t *testing.T, name, timeout types.String) tfsdk.Config {
	t.Helper()
	var schema action.SchemaResponse
	(&nameserverRefreshAction{}).Schema(context.Background(), action.SchemaRequest{}, &schema)
	state := tfsdk.State{Schema: schema.Schema}
	if d := state.Set(context.Background(), &refreshModel{Name: name, Timeout: timeout}); d.HasError() {
		t.Fatal(d)
	}
	return tfsdk.Config{Schema: schema.Schema, Raw: state.Raw}
}
func TestRefreshRepeatedUsesLiveIPs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gets, puts, progress := 0, 0, 0
		a := &nameserverRefreshAction{client: dynadot.NewClient("key", false, options(func(req *http.Request) (*http.Response, error) {
			if req.Method == "GET" {
				gets++
				ip := "192.0.2.1"
				if gets == 2 {
					ip = "192.0.2.2"
				}
				return response(`{"code":200,"data":{"server_name":"ns1.example.com","ip_list":["` + ip + `"]}}`), nil
			}
			puts++
			body, _ := io.ReadAll(req.Body)
			want := "192.0.2.1"
			if puts == 2 {
				want = "192.0.2.2"
			}
			if !strings.Contains(string(body), want) {
				t.Errorf("stale IPs %s", body)
			}
			return response(`{"code":200}`), nil
		})...)}
		for range 2 {
			resp := action.InvokeResponse{SendProgress: func(action.InvokeProgressEvent) { progress++ }}
			a.Invoke(context.Background(), action.InvokeRequest{Config: actionConfig(t, types.StringValue("ns1.example.com"), types.StringNull())}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatal(resp.Diagnostics)
			}
		}
		if gets != 2 || puts != 2 || progress != 2 {
			t.Fatal(gets, puts, progress)
		}
	})
}
func TestRefreshErrors(t *testing.T) {
	for _, scenario := range []string{"missing", "empty", "invalid ip", "put failure", "timeout", "cancelled", "unknown timeout", "invalid timeout"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				puts := 0
				a := &nameserverRefreshAction{client: dynadot.NewClient("key", false, options(func(req *http.Request) (*http.Response, error) {
					if req.Method == "PUT" {
						puts++
						if scenario == "put failure" {
							return response(`{"code":409}`), nil
						}
						return response(`{"code":200}`), nil
					}
					if scenario == "missing" {
						return response(`{"code":404}`), nil
					}
					ips := `"192.0.2.1"`
					if scenario == "empty" {
						ips = ""
					}
					if scenario == "invalid ip" {
						ips = `"broken"`
					}
					return response(`{"code":200,"data":{"server_name":"ns1.example.com","ip_list":[` + ips + `]}}`), nil
				})...)}
				timeout := types.StringNull()
				if scenario == "timeout" {
					timeout = types.StringValue("1ms")
				}
				if scenario == "unknown timeout" {
					timeout = types.StringUnknown()
				}
				if scenario == "invalid timeout" {
					timeout = types.StringValue("0s")
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if scenario == "cancelled" {
					cancel()
				}
				resp := action.InvokeResponse{SendProgress: func(action.InvokeProgressEvent) { t.Error("success emitted on failure") }}
				a.Invoke(ctx, action.InvokeRequest{Config: actionConfig(t, types.StringValue("ns1.example.com"), timeout)}, &resp)
				if !resp.Diagnostics.HasError() {
					t.Fatal("expected error")
				}
				if scenario != "put failure" && puts != 0 {
					t.Fatal("unexpected PUT")
				}
			})
		})
	}
}
func TestUnknownIPRejectedAtApply(t *testing.T) {
	m := model("192.0.2.1")
	m.IPAddresses = types.SetValueMust(types.StringType, []attr.Value{types.StringUnknown()})
	state := resourceState(t, m)
	r := &nameserverResource{}
	resp := resource.CreateResponse{State: state}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan(state)}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("unknown IP accepted")
	}
}

func TestSandboxAndClientConfiguration(t *testing.T) {
	for _, sandbox := range []bool{false, true} {
		host := ""
		p := &dynadotProvider{clientOptions: options(func(req *http.Request) (*http.Response, error) {
			host = req.URL.Host
			return response(`{"code":200,"data":{"account_info":{"price_level":"Regular"}}}`), nil
		})}
		var schema provider.SchemaResponse
		p.Schema(context.Background(), provider.SchemaRequest{}, &schema)
		state := tfsdk.State{Schema: schema.Schema}
		state.Set(context.Background(), &providerModel{APIKey: types.StringValue("key"), Sandbox: types.BoolValue(sandbox)})
		var resp provider.ConfigureResponse
		p.Configure(context.Background(), provider.ConfigureRequest{Config: tfsdk.Config{Schema: schema.Schema, Raw: state.Raw}}, &resp)
		if resp.Diagnostics.HasError() {
			t.Fatal(resp.Diagnostics)
		}
		want := "api.dynadot.com"
		if sandbox {
			want = "api-sandbox.dynadot.com"
		}
		if host != want {
			t.Fatal(host, want)
		}
		r := &nameserverResource{}
		var rr resource.ConfigureResponse
		r.Configure(context.Background(), resource.ConfigureRequest{ProviderData: resp.ResourceData}, &rr)
		a := &nameserverRefreshAction{}
		var ar action.ConfigureResponse
		a.Configure(context.Background(), action.ConfigureRequest{ProviderData: resp.ActionData}, &ar)
		if rr.Diagnostics.HasError() || ar.Diagnostics.HasError() || r.client != a.client {
			t.Fatal("client configuration failed")
		}
	}
	r := &nameserverResource{}
	var rr resource.ConfigureResponse
	r.Configure(context.Background(), resource.ConfigureRequest{ProviderData: "bad"}, &rr)
	if !rr.Diagnostics.HasError() {
		t.Fatal("bad resource data accepted")
	}
	a := &nameserverRefreshAction{}
	var ar action.ConfigureResponse
	a.Configure(context.Background(), action.ConfigureRequest{ProviderData: "bad"}, &ar)
	if !ar.Diagnostics.HasError() {
		t.Fatal("bad action data accepted")
	}
}
func TestUpdateFailuresRetainState(t *testing.T) {
	for _, failure := range []string{"PUT", "GET"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := &nameserverResource{client: dynadot.NewClient("key", false, options(func(req *http.Request) (*http.Response, error) {
					if req.Method == failure {
						return response(`{"code":409}`), nil
					}
					return response(`{"code":200}`), nil
				})...)}
				old := resourceState(t, model("192.0.2.1"))
				next := resourceState(t, model("192.0.2.2"))
				resp := resource.UpdateResponse{State: old}
				r.Update(context.Background(), resource.UpdateRequest{State: old, Plan: tfsdk.Plan(next)}, &resp)
				if !resp.Diagnostics.HasError() {
					t.Fatal("expected failure")
				}
				want := old.Raw
				if failure == "GET" {
					want = next.Raw
				}
				if !resp.State.Raw.Equal(want) {
					t.Fatal("wrong partial state")
				}
			})
		})
	}
}
