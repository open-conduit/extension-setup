// Command setup-extension serves Open Conduit's setup extension, the web
// setup assistant, as a package over protocol version 1. The
// package-manifest and package-certification subcommands print the
// package's open-conduit-extension.json and certification.json.
package main

import (
	"os"

	setup "github.com/open-conduit/extension-setup"
	"github.com/open-conduit/open-conduit/protocol/sdk"
)

func main() {
	if sdk.PackageCommand(os.Args[1:], setup.Manifest(), setup.Certification(), os.Stdout) {
		return
	}
	_ = sdk.Serve(setup.New(setup.BuildOptions{}).Build())
}
