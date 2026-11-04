package execution

import "github.com/zhoushoujianwork/offload/internal/worker"

// NativeControls selects capabilities required by an embedding Host. Nil keeps
// existing delegated-task behavior. Unsupported backends fail before dispatch.
// CallerContext prevents Offload from adding its delegation role or discovering
// project instructions: the caller supplies the complete authorized context.
type NativeControls struct {
	Executable, ProfileDirectory, FreshSessionID, ForkFromSessionID string
	PermissionMode, JSONSchema, SkillDirectory                      string
	CallerContext, Extraction, RestrictedReadOnly                   bool
	CodexAppServer                                                  bool
	StreamEnvironment                                               map[string]string
	SkillRoots                                                      []string
	AllowedTools                                                    []string
	MCPServers                                                      []MCPServer
	Images                                                          []Image
}

type MCPServer struct {
	Name, Command string
	Args          []string
}

type Image struct {
	Name, MediaType string
	Data            []byte
}

func nativeSnapshot(n *NativeControls) *worker.NativeControls {
	if n == nil {
		return nil
	}
	out := &worker.NativeControls{Executable: n.Executable, ProfileDirectory: n.ProfileDirectory,
		FreshSessionID: n.FreshSessionID, ForkFromSessionID: n.ForkFromSessionID,
		PermissionMode: n.PermissionMode, JSONSchema: n.JSONSchema, SkillDirectory: n.SkillDirectory,
		CallerContext: n.CallerContext, Extraction: n.Extraction, RestrictedReadOnly: n.RestrictedReadOnly,
		CodexAppServer: n.CodexAppServer, SkillRoots: append([]string(nil), n.SkillRoots...),
		AllowedTools: append([]string(nil), n.AllowedTools...)}
	out.StreamEnvironment = make(map[string]string, len(n.StreamEnvironment))
	for k, v := range n.StreamEnvironment {
		out.StreamEnvironment[k] = v
	}
	for _, s := range n.MCPServers {
		out.MCPServers = append(out.MCPServers, worker.NativeMCPServer{Name: s.Name, Command: s.Command, Args: append([]string(nil), s.Args...)})
	}
	for _, image := range n.Images {
		out.Images = append(out.Images, worker.NativeImage{Name: image.Name, MediaType: image.MediaType, Data: append([]byte(nil), image.Data...)})
	}
	return out
}
