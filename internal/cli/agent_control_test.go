package cli

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

// A missing dispatch must not turn the app's control command into a model task.
func TestAgentControlDispatchDoesNotLaunchWorker(t *testing.T) {
	root := t.TempDir()
	for key, dir := range map[string]string{
		"HOME": "home", "CODEX_HOME": "codex", "CLAUDE_CONFIG_DIR": "claude",
		"XDG_CONFIG_HOME": "config", "XDG_STATE_HOME": "state",
	} {
		t.Setenv(key, filepath.Join(root, dir))
	}
	t.Setenv("OFFLOAD_PROFILE", "")
	for _, args := range [][]string{
		{"agent", "status", "--json"},
		{"agent", "disable", "--agent", "codex", "--json"},
	} {
		var out, stderr bytes.Buffer
		if code := Run("test", args, bytes.NewReader(nil), &out, &stderr); code != 0 {
			t.Fatalf("%v: exit %d: %s", args, code, stderr.String())
		}
		var result struct {
			SchemaVersion int               `json:"schema_version"`
			Agents        []json.RawMessage `json:"agents"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.SchemaVersion != 1 || len(result.Agents) == 0 {
			t.Fatalf("control response, not worker output, required: %s (%v)", out.String(), err)
		}
	}
}
