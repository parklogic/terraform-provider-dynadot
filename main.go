package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/parklogic/terraform-provider-dynadot/internal/provider"
)

//go:generate go tool tfplugindocs generate --provider-name dynadot

var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "Run with debugger support")
	flag.Parse()
	if err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{Address: "registry.terraform.io/parklogic/dynadot", Debug: debug}); err != nil {
		log.Fatal(err)
	}
}
