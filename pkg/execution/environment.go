package execution

import (
	"path/filepath"

	"github.com/zhoushoujianwork/offload/internal/worker"
)

// Environment is a private persistent native runtime and empty workspace.
// Keep Root to resume its sessions. It is not a filesystem or network sandbox.
type Environment struct{ Root, Workspace string }

// NewEnvironment creates a fresh environment without starting a provider or
// reading credentials. Pass Root as Request.EnvironmentDirectory and Workspace
// as Request.Directory. Authentication is seeded on the first execution only;
// subsequent native refreshes stay inside this environment.
func NewEnvironment() (Environment, error) {
	root, err := worker.NewEnvironment()
	if err != nil {
		return Environment{}, err
	}
	return Environment{Root: root, Workspace: filepath.Join(root, "workspace")}, nil
}
