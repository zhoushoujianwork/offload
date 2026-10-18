// offload delegates tasks to a configured CLI worker or opens interactive settings.
package main

import (
	"os"

	"github.com/zhoushoujianwork/offload/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(cli.Run(version, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
