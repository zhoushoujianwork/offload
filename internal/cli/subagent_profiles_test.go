package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
)

func TestUseForProfileSavePatchClear(t *testing.T) {
	profileCfg(t, "")
	for _, tc := range []struct{ request, want string }{
		{`{"name":"cx","create":true,"backend":"codex","use_for":"  frontend/UI; long reviews  "}`, "frontend/UI; long reviews"},
		{`{"name":"cx","model":"new"}`, "frontend/UI; long reviews"},
		{`{"name":"cx","use_for":null}`, "frontend/UI; long reviews"},
		{`{"name":"cx","use_for":"backend"}`, "backend"},
		{`{"name":"cx","use_for":""}`, ""},
		{`{"name":"cx","use_for":"   "}`, ""},
	} {
		code, out, stderr := runProfileCLI([]string{"profile", "save", "--json"}, tc.request)
		if code != 0 {
			t.Fatalf("save: %d %s", code, stderr)
		}
		if got := decodeSnap(t, out).Profiles[0].UseFor; got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
		code, out, stderr = runProfileCLI([]string{"profiles", "--json"}, "")
		if code != 0 || !strings.Contains(out, `"use_for":`) || decodeSnap(t, out).Profiles[0].UseFor != tc.want {
			t.Fatalf("list: %d %s %s", code, out, stderr)
		}
		cfg, err := config.Load(config.Path())
		if err != nil || cfg.Profiles["cx"].UseFor != tc.want {
			t.Fatalf("stored: %+v %v", cfg, err)
		}
	}
	for _, value := range []string{"a\nb", "a\tb", strings.Repeat("界", 301)} {
		before, _ := os.ReadFile(config.Path())
		request, _ := json.Marshal(map[string]any{"name": "cx", "use_for": value})
		code, _, stderr := runProfileCLI([]string{"profile", "save", "--json"}, string(request))
		after, _ := os.ReadFile(config.Path())
		if code != 2 || !strings.Contains(stderr, "use_for") || string(before) != string(after) {
			t.Fatalf("invalid hint: %d %s", code, stderr)
		}
	}
}

func TestUseForTextListing(t *testing.T) {
	profileCfg(t, `{"default":"cx","profiles":{"cx":{"backend":"codex","use_for":"frontend/UI work"}}}`)
	code, out, stderr := runProfileCLI([]string{"profiles"}, "")
	if code != 0 || !strings.Contains(out, " — frontend/UI work\n") || strings.Count(out, "\n") != 1 {
		t.Fatal(code, out, stderr)
	}
}

func TestClaudeProfileSaveEffortValidation(t *testing.T) {
	profileCfg(t, profileListCfg)
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max", "ultra", "wrong", ""} {
		request, _ := json.Marshal(map[string]string{"name": "zeta", "reasoning_effort": effort})
		code, _, stderr := runProfileCLI([]string{"profile", "save", "--json"}, string(request))
		want := 0
		if effort == "ultra" || effort == "wrong" {
			want = 2
		}
		if code != want {
			t.Fatalf("%s: %d %s", effort, code, stderr)
		}
	}
}
