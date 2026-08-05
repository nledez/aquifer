// Package cli wires the aquifer subcommands together.
package cli

import (
	"fmt"
	"io"
)

// version is set at build time via -ldflags.
var version = "dev"

const usage = `aquifer - distributed APT mirror with a content-addressed cache

Usage:
  aquifer <command> [flags]

Commands:
  serve      Serve apt clients from a local cache backed by object storage
  publish    Publish an aptly publication to object storage
  gc         Delete blobs no longer referenced by retained revisions
  ping       Check the local instance readiness and exit 0 or 1
  version    Print the version and exit
`

// Main dispatches a subcommand and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	case "serve", "publish", "gc", "ping":
		fmt.Fprintf(stderr, "aquifer: %s is not implemented yet\n", args[0])
		return 2
	default:
		fmt.Fprintf(stderr, "aquifer: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
