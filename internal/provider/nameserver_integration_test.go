//go:build integration

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/hashicorp/terraform-plugin-sdk/v2/plugin"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/parklogic/terraform-provider-dynadot/internal/dynadot"
)

type simulatedDynadot struct {
	mu                      sync.Mutex
	names                   map[string][]string
	writes                  []string
	failSet, conflictDelete bool
}

func (s *simulatedDynadot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	reply := func(code int, data any) { _ = json.NewEncoder(w).Encode(map[string]any{"code": code, "data": data}) }
	if r.URL.Path == "/accounts/info" {
		reply(200, map[string]any{"account_info": map[string]string{"price_level": "Super Bulk"}})
		return
	}
	name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/nameservers/"), "/set_ip")
	switch r.Method {
	case "POST":
		var body struct {
			NS struct {
				Name string `json:"server_name"`
				IP   string `json:"ip"`
			} `json:"name_server"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			reply(400, nil)
			return
		}
		s.names[body.NS.Name] = []string{body.NS.IP}
		s.writes = append(s.writes, "register:"+body.NS.Name+":"+body.NS.IP)
		reply(200, nil)
	case "PUT":
		if s.failSet {
			reply(400, nil)
			return
		}
		var body struct {
			IPs []string `json:"ip_list"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.names[name] = body.IPs
		s.writes = append(s.writes, "set:"+name+":"+strings.Join(body.IPs, ","))
		reply(200, nil)
	case "GET":
		ips, ok := s.names[name]
		if !ok {
			reply(404, nil)
			return
		}
		reply(200, map[string]any{"server_name": name, "ip_list": ips})
	case "DELETE":
		if s.conflictDelete {
			reply(409, nil)
			return
		}
		if _, ok := s.names[name]; !ok {
			reply(404, nil)
			return
		}
		delete(s.names, name)
		s.writes = append(s.writes, "delete:"+name)
		reply(200, nil)
	default:
		reply(400, nil)
	}
}
func integrationFactory(t *testing.T) (*simulatedDynadot, map[string]func() (tfprotov6.ProviderServer, error)) {
	t.Helper()
	s := &simulatedDynadot{names: map[string][]string{}}
	server := httptest.NewServer(s)
	t.Cleanup(server.Close)
	return s, map[string]func() (tfprotov6.ProviderServer, error){"dynadot": func() (tfprotov6.ProviderServer, error) {
		return providerserver.NewProtocol6WithError(&dynadotProvider{version: "test", clientOptions: []dynadot.Option{dynadot.WithEndpoint(server.URL)}})()
	}}
}
func integrationConfig(name string, ips ...string) string {
	encoded, _ := json.Marshal(ips)
	return fmt.Sprintf("provider \"dynadot\" { api_key = \"local-test\" }\nresource \"dynadot_nameserver\" \"test\" {\nname = %q\nip_addresses = %s\n}\n", name, encoded)
}
func (s *simulatedDynadot) check(writes ...string) resource.TestCheckFunc {
	return func(_ *terraform.State) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !reflect.DeepEqual(s.writes, writes) {
			return fmt.Errorf("writes = %v, want %v", s.writes, writes)
		}
		return nil
	}
}
func TestIntegrationNameserverLifecycle(t *testing.T) {
	s, f := integrationFactory(t)
	name := "ns1.example.com"
	config := integrationConfig(name, "192.0.2.1")
	resource.Test(t, resource.TestCase{IsUnitTest: true, ProtoV6ProviderFactories: f, CheckDestroy: func(_ *terraform.State) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.names) != 0 {
			return fmt.Errorf("leaked nameservers: %v", s.names)
		}
		return nil
	}, Steps: []resource.TestStep{
		{Config: config, Check: resource.ComposeTestCheckFunc(resource.TestCheckResourceAttr("dynadot_nameserver.test", "name", name), resource.TestCheckNoResourceAttr("dynadot_nameserver.test", "id"), s.check("register:"+name+":192.0.2.1", "set:"+name+":192.0.2.1"))},
		{Config: config, PlanOnly: true, Check: s.check("register:"+name+":192.0.2.1", "set:"+name+":192.0.2.1")},
		{ResourceName: "dynadot_nameserver.test", ImportState: true, ImportStateId: name, ImportStateVerify: true, ImportStateVerifyIdentifierAttribute: "name"},
		{Config: integrationConfig(name, "192.0.2.2", "2001:db8::1")},
		{PreConfig: func() { s.mu.Lock(); s.names[name] = []string{"192.0.2.3"}; s.mu.Unlock() }, Config: integrationConfig(name, "192.0.2.2", "2001:db8::1"), Check: resource.TestCheckResourceAttr("dynadot_nameserver.test", "ip_addresses.#", "2")},
		{PreConfig: func() { s.mu.Lock(); delete(s.names, name); s.mu.Unlock() }, Config: config},
		{Config: integrationConfig("ns2.example.com", "192.0.2.1")},
	}})
}
func TestIntegrationNameserverEquivalentRepresentations(t *testing.T) {
	s, f := integrationFactory(t)
	config := integrationConfig("NS1.Example.COM.", "2001:0db8:0000:0000:0000:0000:0000:0001", "192.0.2.1")
	resource.Test(t, resource.TestCase{IsUnitTest: true, ProtoV6ProviderFactories: f, Steps: []resource.TestStep{
		{Config: config, Check: s.check("register:ns1.example.com:192.0.2.1", "set:ns1.example.com:192.0.2.1,2001:db8::1")},
		{Config: config, PlanOnly: true, Check: s.check("register:ns1.example.com:192.0.2.1", "set:ns1.example.com:192.0.2.1,2001:db8::1")},
	}})
}
func TestIntegrationPartialCreateRetainsState(t *testing.T) {
	s, f := integrationFactory(t)
	s.failSet = true
	config := integrationConfig("ns1.example.com", "192.0.2.1", "192.0.2.2")
	resource.Test(t, resource.TestCase{IsUnitTest: true, ProtoV6ProviderFactories: f, Steps: []resource.TestStep{
		{Config: config, ExpectError: regexp.MustCompile("(?s)(IP|address|400)")},
		{PreConfig: func() { s.mu.Lock(); s.failSet = false; s.mu.Unlock() }, Config: config, Destroy: true},
	}})
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.names) != 0 {
		t.Fatal("Terraform failed to track and destroy partial registration")
	}
}
func TestIntegrationDeleteConflict(t *testing.T) {
	s, f := integrationFactory(t)
	config := integrationConfig("ns1.example.com", "192.0.2.1")
	resource.Test(t, resource.TestCase{IsUnitTest: true, ProtoV6ProviderFactories: f, Steps: []resource.TestStep{
		{Config: config},
		{PreConfig: func() { s.mu.Lock(); s.conflictDelete = true; s.mu.Unlock() }, Config: config, Destroy: true, ExpectError: regexp.MustCompile("409")},
		{PreConfig: func() { s.mu.Lock(); s.conflictDelete = false; s.mu.Unlock() }, Config: config, Destroy: true},
		{Config: config, Destroy: true},
	}})
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.names) != 0 {
		t.Fatal("conflict lost tracked state")
	}
}
func TestIntegrationRefreshLifecycle(t *testing.T) {
	s, f := integrationFactory(t)
	config := func(revision string) string {
		return integrationConfig("ns1.example.com", "192.0.2.1") + fmt.Sprintf(`
action "dynadot_nameserver_refresh" "test" {
 config { name = dynadot_nameserver.test.name }
}
resource "terraform_data" "trigger" {
 input = %q
 lifecycle {
  action_trigger {
   events = [after_create, after_update]
   actions = [action.dynadot_nameserver_refresh.test]
  }
 }
}
`, revision)
	}
	resource.Test(t, resource.TestCase{IsUnitTest: true, ProtoV6ProviderFactories: f, Steps: []resource.TestStep{
		{Config: config("one"), Check: s.check("register:ns1.example.com:192.0.2.1", "set:ns1.example.com:192.0.2.1", "set:ns1.example.com:192.0.2.1")},
		{Config: config("two"), Check: s.check("register:ns1.example.com:192.0.2.1", "set:ns1.example.com:192.0.2.1", "set:ns1.example.com:192.0.2.1", "set:ns1.example.com:192.0.2.1")},
	}})
}

func TestIntegrationRefreshDirectInvocation(t *testing.T) {
	s, f := integrationFactory(t)
	s.names["ns1.example.com"] = []string{"192.0.2.1"}
	server, err := f["dynadot"]()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cfg, closed, err := plugin.DebugServe(ctx, &plugin.ServeOpts{ProviderAddr: "registry.terraform.io/hashicorp/dynadot", GRPCProviderV6Func: func() tfprotov6.ProviderServer { return server }})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
			t.Error("provider failed to stop")
		}
	})
	reattach, err := json.Marshal(map[string]any{"registry.terraform.io/hashicorp/dynadot": cfg})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	err = os.WriteFile(filepath.Join(dir, "main.tf"), []byte(`provider "dynadot" { api_key = "local-test" }
action "dynadot_nameserver_refresh" "test" {
 config { name = "ns1.example.com" }
}
`), 0600)
	if err != nil {
		t.Fatal(err)
	}
	binary := os.Getenv("TF_ACC_TERRAFORM_PATH")
	if binary == "" {
		binary = "terraform"
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "TF_REATTACH_PROVIDERS="+string(reattach), "TF_IN_AUTOMATION=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("terraform %v: %v\n%s", args, err, out)
		}
	}
	for _, ip := range []string{"192.0.2.1", "192.0.2.2"} {
		s.mu.Lock()
		s.names["ns1.example.com"] = []string{ip}
		s.mu.Unlock()
		run("apply", "-auto-approve", "-input=false", "-invoke=action.dynadot_nameserver_refresh.test")
	}
	if err := s.check("set:ns1.example.com:192.0.2.1", "set:ns1.example.com:192.0.2.2")(nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "terraform.tfstate"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Resources []json.RawMessage `json:"resources"`
	}
	if err = json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Resources) != 0 {
		t.Fatal("action persisted a resource")
	}
}
