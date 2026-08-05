// Command aquifer is the single static binary for every Aquifer role:
// publishing to object storage, serving edges, collecting garbage, and
// health-checking a local instance.
package main

import (
	"os"

	"github.com/nledez/aquifer/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
