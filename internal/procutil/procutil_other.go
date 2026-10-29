//go:build !unix

package procutil

import "os/exec"

// configure keeps exec.CommandContext semantics: only the direct child is killed.
func configure(*exec.Cmd) {}
