//go:build acceptance

package provider

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/parklogic/terraform-provider-dynadot/internal/dynadot"
)

// TestAccSandboxNameserver requires a sandbox API key with API access enabled,
// the runner IP allowlisted, and a domain owned by that sandbox account.
func TestAccSandboxNameserver(t *testing.T) {
	if os.Getenv("TF_ACC") != "1" {
		t.Skip("set TF_ACC=1 to enable sandbox acceptance tests")
	}
	key, domain := os.Getenv("DYNADOT_API_KEY"), os.Getenv("DYNADOT_TEST_DOMAIN")
	if key == "" || domain == "" {
		t.Skip("DYNADOT_API_KEY (sandbox key) and DYNADOT_TEST_DOMAIN are required")
	}
	name := fmt.Sprintf("tfacc-%x.%s", time.Now().UnixNano(), dynadot.NormalizeHostname(domain))
	client := dynadot.NewClient(key, true)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := client.DeleteNameserver(ctx, name); err != nil && !dynadot.IsNotFound(err) {
			t.Errorf("sandbox cleanup %s: %v", name, err)
		}
	})
	config := func(ip string) string {
		return fmt.Sprintf(`
provider "dynadot" { sandbox = true }
resource "dynadot_nameserver" "test" {
 name = %q
 ip_addresses = [%q]
}
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
`, name, ip, ip)
	}
	resource.Test(t, resource.TestCase{ProtoV6ProviderFactories: map[string]func() (tfprotov6.ProviderServer, error){"dynadot": providerserver.NewProtocol6WithError(New("test")())}, CheckDestroy: func(_ *terraform.State) error {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, err := client.GetNameserver(ctx, name)
		if dynadot.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("nameserver %s still exists", name)
	}, Steps: []resource.TestStep{
		{Config: config("192.0.2.1"), Check: resource.TestCheckResourceAttr("dynadot_nameserver.test", "name", name)},
		{ResourceName: "dynadot_nameserver.test", ImportState: true, ImportStateId: name, ImportStateVerify: true, ImportStateVerifyIdentifierAttribute: "name"},
		{Config: config("192.0.2.2"), Check: resource.TestCheckResourceAttr("dynadot_nameserver.test", "ip_addresses.#", "1")},
		{Config: config("192.0.2.2"), PlanOnly: true},
	}})
}
