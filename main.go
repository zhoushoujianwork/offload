// offload runs a task on a cheaper model through `claude -p` against an Anthropic-compatible
// gateway and prints only the worker's final report, so a supervising agent spends few tokens.
package main

import (
	"os"

	"github.com/zhoushoujianwork/offload/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(cli.Run(version, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
