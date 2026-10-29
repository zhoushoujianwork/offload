// Package procutil starts child processes that stop completely when their context ends.
package procutil

import (
	"context"
	"os/exec"
)

// CommandContext is exec.CommandContext, except that on Unix the child leads its own process group
// and context cancellation kills that whole group. Descendants of the child therefore do not outlive
// a cancelled run. A child that exits on its own is never followed by a group kill, so background
// services it intentionally left running keep running. A descendant that moves itself into another
// session or group is out of reach. Other platforms keep the exec.CommandContext behavior.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	configure(cmd)
	return cmd
}
