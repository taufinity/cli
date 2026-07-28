// terraform-provider-taufinity serves the Taufinity Studio Terraform provider.
// It is a thin frontend over github.com/taufinity/cli/pkg/studioadmin — the same
// code path the CLI uses. It talks to the Studio admin API directly; it never
// invokes the taufinity CLI binary.
package main

import (
	"context"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/taufinity/cli/internal/tfprovider"
)

func main() {
	err := providerserver.Serve(context.Background(), tfprovider.New, providerserver.ServeOpts{
		Address: "registry.terraform.io/taufinity/taufinity",
	})
	if err != nil {
		log.Fatal(err)
	}
}
