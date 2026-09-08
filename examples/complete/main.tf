terraform {
  required_version = ">= 1.14.0"
  required_providers {
    dynadot = {
      source = "parklogic/dynadot"
    }
  }
}

provider "dynadot" {}

variable "refresh_token" {
  type        = string
  default     = "initial"
  description = "Change this value to invoke the lifecycle refresh action."
}

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

resource "terraform_data" "refresh" {
  input = var.refresh_token

  lifecycle {
    action_trigger {
      events  = [after_create, after_update]
      actions = [action.dynadot_nameserver_refresh.primary]
    }
  }
}
