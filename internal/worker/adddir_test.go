package worker

import (
	"context"
	"slices"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
)

func TestClaudeAddDirArgsPrecedeKnownFlagAndTask(t *testing.T) {
	for _, resume := range []string{"", "sess"} {
		o := Opts{Resume: resume, AddDirs: []string{"/a b", "/-x"}, Task: "do it"}
		args := buildArgs(o, "/p")
		want := []string{"-p", "--bare", "--setting-sources", "", "--output-format", "stream-json", "--verbose"}
		if resume != "" {
			want = append(want, "--resume", resume)
		}
		want = append(want, "--add-dir", "/a b", "--add-dir", "/-x", "--dangerously-skip-permissions", "--append-system-prompt-file", "/p", "do it")
		if !slices.Equal(args, want) {
			t.Fatalf("%q != %q", args, want)
		}
	}
	if slices.Contains(buildArgs(Opts{}, "/p"), "--add-dir") {
		t.Fatal("no dirs, no flag")
	}
}

func TestCodexAddDirArgsPrecedeResume(t *testing.T) {
	p := config.Profile{ReasoningEffort: "high", ServiceTier: "fast"}
	for _, resume := range []string{"", "sess"} {
		args := buildCodexArgs(Opts{Profile: p, Model: "m", Resume: resume, AddDirs: []string{"/a", "/b"}})
		want := []string{"exec", "--dangerously-bypass-approvals-and-sandbox", "-c", `model_reasoning_effort="high"`, "-c", `service_tier="fast"`, "--add-dir", "/a", "--add-dir", "/b"}
		if resume != "" {
			want = append(want, "resume")
		}
		want = append(want, "--json", "--skip-git-repo-check", "-m", "m")
		if resume != "" {
			want = append(want, resume)
		}
		want = append(want, "-")
		if !slices.Equal(args, want) {
			t.Fatalf("%q != %q", args, want)
		}
	}
}

func TestSalvageKeepsAddDirsAndRecordCopiesThem(t *testing.T) {
	o := Opts{AddDirs: []string{"/a"}}
	so := o
	so.Resume, so.Salvage = "s", true
	args := buildArgs(so, "/p")
	if i := slices.Index(args, "--add-dir"); i < 0 || args[i+1] != "/a" || !slices.Contains(args, "--tools") {
		t.Fatalf("%q", args)
	}

	t.Setenv("OFFLOAD_CLAUDE", fakeResultClaude(t, measuredFresh))
	mockPreflight(t)
	stateEnv(t)
	o = testOpts(t)
	o.AddDirs = []string{"/x"}
	rec, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rec.AddDirs, []string{"/x"}) {
		t.Fatalf("%+v", rec.AddDirs)
	}
	o.AddDirs[0] = "/mutated"
	if rec.AddDirs[0] != "/x" {
		t.Fatal("record aliases Opts")
	}
}
