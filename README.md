# Terraform provider for Dynadot

Manage registered nameservers with Dynadot's REST v1 API. Requires Terraform 1.14+ (for actions) and Go 1.27 to build. The provider address is `parklogic/dynadot`. Until the first Registry release is published, use a local development override below.

The provider manages nameserver registrations and their IP addresses. External nameservers, domain delegation, and DNS records are outside its scope. The parent domain must meet Dynadot's registration requirements for child nameservers.

```hcl
terraform {
  required_version = ">= 1.14.0"
  required_providers {
    dynadot = { source = "parklogic/dynadot" }
  }
}

provider "dynadot" {} # Uses DYNADOT_API_KEY.

resource "dynadot_nameserver" "primary" {
  name         = "ns1.example.com"
  ip_addresses = ["192.0.2.10", "2001:db8::10"]
}

action "dynadot_nameserver_refresh" "primary" {
  config {
    name    = dynadot_nameserver.primary.name
    timeout = "5m"
  }
}
```

Set `DYNADOT_API_KEY`, or supply the optional sensitive `api_key` provider attribute. Explicit configuration takes precedence. Set `sandbox = true` and use a sandbox API key for `https://api-sandbox.dynadot.com/restful/v1`; production uses `https://api.dynadot.com/restful/v1`. Credentials and account responses are never logged by this provider.

Import an existing registration by hostname:

```sh
terraform import dynadot_nameserver.primary ns1.example.com
```

Changing `name` replaces the registration. Creation registers one deterministically selected IP, then sets the complete address set. If the latter step fails, Terraform retains the registration in state for recovery. Deleting an in-use nameserver fails and retains state; remove its dependencies before retrying. After an ambiguous registration failure, inspect Dynadot and import the hostname if registration succeeded before retrying.

Normal reads and unchanged applies do not resend IPs. Invoke the refresh action explicitly to fetch the live IPs and resend them through `set_ip`, even when unchanged:

```sh
terraform apply -invoke=action.dynadot_nameserver_refresh.primary
```

The refresh action uses the live address set, not the resource configuration, and has no state. Missing registrations or invalid/empty live IP lists fail. Success means the update request succeeded. The intended use is the user-supplied Dynadot retention behavior; no retention duration has been verified or is guaranteed here. Periodic refresh requires an external scheduler invoking Terraform. Use the usual Terraform state locking to serialize scheduled runs.

For lifecycle invocation, see [the complete example](examples/complete/main.tf). Its `terraform_data` resource invokes refresh after creation and whenever `refresh_token` changes. An unchanged apply does not trigger it:

```sh
terraform apply -var="refresh_token=$(date -u +%FT%TZ)"
```

## Automatic account limits

Each provider configuration calls `GET /accounts/info` and reads `data.account_info.price_level`. Discovery begins at Regular limits; the resulting client and limits are shared by resources and actions for that configuration.

| Tier | Requests/second | Requests/minute | Concurrent requests |
| --- | ---: | ---: | ---: |
| Regular | 1 | 60 | 1 |
| Bulk | 10 | 600 | 5 |
| Super Bulk | 100 | 6,000 | 35 |

Requests are spaced without accumulated bursts and in-flight requests are separately bounded, including retries. Tier names ignore case, whitespace, hyphens, and underscores. A missing or unknown tier emits a warning and uses Regular limits. Discovery errors fail configuration after bounded retries. No account data is persisted in Terraform state, and subsequent configurations rediscover the tier.

HTTP or API-code 429 responses apply a shared cooldown using `Retry-After`, defaulting to 60 seconds when absent. Waits respect cancellation and deadlines. Separate provider processes and other API consumers share the account quota; this local limiter cannot coordinate them.

API contracts and limits: [Dynadot REST API documentation](https://www.dynadot.com/domain/api-document) (select v1).

## Development

```sh
go build -o "$PWD/bin/terraform-provider-dynadot" .
```

Create a Terraform CLI configuration file, replacing the path with this checkout's absolute `bin` path:

```hcl
provider_installation {
  dev_overrides {
    "parklogic/dynadot" = "/absolute/path/to/terraform-provider-dynadot/bin"
  }
  direct {}
}
```

Set `TF_CLI_CONFIG_FILE` to that file, then run Terraform from `examples/complete`. With a development override and no external dependencies, skip `terraform init`; `terraform validate`, `plan`, and `apply` use the local executable. Do not apply the example against production without replacing its hostname and addresses.

```sh
gofmt -w main.go internal
go vet ./...
go test -race ./...
go build ./...
go test -race -tags=integration ./internal/provider -timeout 10m
go generate ./...
terraform fmt -check -recursive examples
```

Integration tests run Terraform against a local simulated HTTP API and do not require Dynadot credentials. Race tests require a C compiler and `CGO_ENABLED=1`; `make check` sets this automatically.

Sandbox acceptance tests require a sandbox API key with API access enabled, the runner IP allowlisted, and a domain owned by the sandbox account. Set `DYNADOT_API_KEY` to that sandbox key and `DYNADOT_TEST_DOMAIN` to the domain, then run:

```sh
TF_ACC=1 CGO_ENABLED=1 go test -race -tags=acceptance ./internal/provider -run TestAcc -timeout 20m
```

The test forces `sandbox = true`, creates a unique child hostname, exercises creation/import/update/refresh, and attempts cleanup even after a failure. Missing prerequisites skip the sandbox test. Do not use production credentials.

## License

Mozilla Public License 2.0; see [LICENSE](LICENSE).
