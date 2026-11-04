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

func TestCodexHostInstructionProfileRemainsEnabledInIsolatedHome(t *testing.T) {
	o := Opts{Native: &NativeControls{CallerContext: true, PermissionMode: "bypassPermissions"}, codexHostProfile: "private-instructions", EnvironmentDirectory: "/owned/environment"}
	args := strings.Join(buildCodexArgs(o), "\n")
	if !strings.Contains(args, "--profile\nprivate-instructions") || strings.Contains(args, "--ignore-user-config") {
		t.Fatal("explicit isolated profile would be suppressed")
	}
	o.EnvironmentDirectory = ""
	if !strings.Contains(strings.Join(buildCodexArgs(o), "\n"), "--ignore-user-config") {
		t.Fatal("ambient user config would be loaded")
	}
}
