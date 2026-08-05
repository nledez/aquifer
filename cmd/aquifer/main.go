// Command aquifer is the single static binary for every Aquifer role:
// publishing to object storage, serving edges, collecting garbage, and
// health-checking a local instance.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/nledez/aquifer/internal/cli"
)

func main() {
	// A publication or a GC run can take minutes. Cancelling on a signal lets
	// it stop between operations rather than being killed mid-upload.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Main(ctx, os.Args[1:], os.Stdout, os.Stderr))
}
