package worker

import (
	"strings"
	"testing"
)

func TestCodexHostControlsRejectUnsupportedRestrictions(t *testing.T) {
	for _, change := range []func(*NativeControls){
		func(n *NativeControls) { n.RestrictedReadOnly = true },
		func(n *NativeControls) { n.AllowedTools = []string{"Read"} },
		func(n *NativeControls) { n.Extraction = true },
		func(n *NativeControls) { n.SkillDirectory = "/skills" },
	} {
		n := &NativeControls{PermissionMode: "bypassPermissions"}
		change(n)
		if n.Validate("codex", "", false) == nil {
			t.Fatal("unsupported Host policy accepted")
		}
	}
	n := &NativeControls{PermissionMode: "bypassPermissions"}
	if n.Validate("codex", "", true) == nil {
		t.Fatal("tool-free execution accepted")
	}
	if err := n.Validate("codex", "", false); err != nil {
		t.Fatal(err)
	}
}

func TestCodexHostMCPReplacesAmbientConfiguration(t *testing.T) {
	n := &NativeControls{PermissionMode: "bypassPermissions", MCPServers: []NativeMCPServer{{Name: "test", Command: "/test/server", Args: []string{"value with spaces"}}}}
	args := strings.Join(buildCodexArgs(Opts{Native: n}), "\n")
	for _, expected := range []string{"--ignore-user-config", "mcp_servers={\"test\"={command=\"/test/server\",args=[\"value with spaces\"],required=true}}", "project_doc_max_bytes=0"} {
		if !strings.Contains(args, expected) {
			t.Fatalf("missing %q in %s", expected, args)
		}
	}
}
