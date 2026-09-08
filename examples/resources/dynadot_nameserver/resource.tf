resource "dynadot_nameserver" "primary" {
  name         = "ns1.example.com"
  ip_addresses = ["192.0.2.10", "2001:db8::10"]
}
