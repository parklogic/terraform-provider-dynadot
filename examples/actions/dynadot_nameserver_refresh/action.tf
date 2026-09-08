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
