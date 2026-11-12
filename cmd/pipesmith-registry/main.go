// Command pipesmith-registry serves file blob stores and directory trees read only.
package main

import (
	"os"

	"github.com/iogentic/pipesmith-registry/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
