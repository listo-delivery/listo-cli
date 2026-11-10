// Command listo is the command-line tool for developers on the Listo Partner
// Delivery API. See cli/README.md.
package main

import (
	"os"

	"github.com/listo-delivery/listo-cli/internal/cmd"
	"github.com/listo-delivery/listo-cli/internal/output"
)

func main() {
	os.Exit(cmd.Execute(os.Args[1:], output.StdStreams(), os.Getenv))
}
