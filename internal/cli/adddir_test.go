package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/history"
)

func mkdirs(t *testing.T, names ...string) []string {
	t.Helper()
	base, _ := filepath.EvalSymlinks(t.TempDir())
	var out []string
	for _, n := range names {
		p := filepath.Join(base, n)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// argv returns the fake worker's recorded arguments, one per line (ENV line dropped).
func argv(t *testing.T, record string) []string {
	t.Helper()
	b, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	var out []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "ENV ") {
			out = append(out, l)
		}
	}
	return out
}

func addDirArgs(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "--add-dir" && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

func lastRecord(t *testing.T) history.Record {
	t.Helper()
	rs, err := history.Read(history.Path())
	if err != nil || len(rs) == 0 {
		t.Fatalf("history: %v %v", rs, err)
	}
	return rs[len(rs)-1]
}

func TestAddDirRepeatNormalizeDedupeOrderAndPrimary(t *testing.T) {
	work, record := setup(t, okReply)
	dirs := mkdirs(t, "a b", "-odd", "c")
	// Relative paths resolve against the invocation directory, not the worker's -d directory.
	t.Chdir(filepath.Dir(dirs[0]))
	code, _, e := runCLI("-d", work, "--add-dir", "a b", "--add-dir=-odd", "--add-dir", "./c/../c", "--add-dir", dirs[0], "--add-dir", work, "task here")
	if code != 0 {
		t.Fatal(e)
	}
	args := argv(t, record)
	if got := addDirArgs(args); !slices.Equal(got, dirs) {
		t.Fatalf("add-dir args %q want %q", got, dirs)
	}
	if args[len(args)-1] != "task here" {
		t.Fatalf("task must stay last: %q", args)
	}
	pf := slices.Index(args, "--append-system-prompt-file")
	for i, a := range args {
		if a == "--add-dir" && i > pf {
			t.Fatalf("--add-dir after --append-system-prompt-file: %q", args)
		}
	}
	if r := lastRecord(t); !slices.Equal(r.AddDirs, dirs) || r.Dir != work {
		t.Fatalf("record: %+v", r)
	}
}

func TestAddDirInvalidValuesFailBeforeTokenOrWorker(t *testing.T) {
	work, record := setup(t, okReply)
	marker := filepath.Join(t.TempDir(), "token-ran")
	cfg := `{"default":"cheap","profiles":{"cheap":{"base_url":"https://gw.example/anthropic","model":"m-1","token_cmd":"touch ` + marker + `; echo tok"}}}`
	if err := os.WriteFile(config.Path(), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(work, "file")
	os.WriteFile(file, nil, 0o644)
	for name, args := range map[string][]string{
		"empty":    {"--add-dir", ""},
		"missing":  {"--add-dir", filepath.Join(work, "nope")},
		"file":     {"--add-dir", file},
		"conflict": {"--add-dir", work, "--clear-add-dirs"},
	} {
		t.Run(name, func(t *testing.T) {
			code, out, e := runCLI(append(append([]string{"-d", work}, args...), "task")...)
			if code != 2 {
				t.Fatalf("code=%d out=%q err=%q", code, out, e)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("token_cmd ran")
			}
			if _, err := os.Stat(record); err == nil {
				t.Fatal("worker ran")
			}
		})
	}
}

func TestAddDirResumeInheritsLatestReplacesAndClears(t *testing.T) {
	work, record := setup(t, okReply)
	d := mkdirs(t, "a", "b")
	if code, _, e := runCLI("-d", work, "--add-dir", d[0], "first"); code != 0 {
		t.Fatal(e)
	}
	// Inherit.
	if code, _, e := runCLI("-r", "s-1", "second"); code != 0 {
		t.Fatal(e)
	}
	if got := addDirArgs(argv(t, record)); !slices.Equal(got, d[:1]) {
		t.Fatalf("inherit: %q", got)
	}
	// Explicit replacement (not a merge), then a later resume inherits the latest, not the first record.
	if code, _, e := runCLI("-r", "s-1", "--add-dir", d[1], "third"); code != 0 {
		t.Fatal(e)
	}
	if got := addDirArgs(argv(t, record)); !slices.Equal(got, d[1:]) {
		t.Fatalf("replace: %q", got)
	}
	if code, _, e := runCLI("-r", "s-1", "fourth"); code != 0 {
		t.Fatal(e)
	}
	if got := addDirArgs(argv(t, record)); !slices.Equal(got, d[1:]) {
		t.Fatalf("latest, not first: %q", got)
	}
	// Explicit clear persists as empty and stays empty.
	if code, _, e := runCLI("-r", "s-1", "--clear-add-dirs", "fifth"); code != 0 {
		t.Fatal(e)
	}
	if got := addDirArgs(argv(t, record)); len(got) != 0 || len(lastRecord(t).AddDirs) != 0 {
		t.Fatalf("clear: %q %+v", got, lastRecord(t))
	}
	if code, _, e := runCLI("-r", "s-1", "sixth"); code != 0 {
		t.Fatal(e)
	}
	if got := addDirArgs(argv(t, record)); len(got) != 0 {
		t.Fatalf("clear must persist: %q", got)
	}
}

func TestAddDirOldHistoryWithoutFieldResumesEmpty(t *testing.T) {
	work, record := setup(t, okReply)
	line := `{"ts":"2026-10-01 10:00:00","status":"ok","profile":"cheap","model":"m-1","dir":"` + work + `","task":"old","session":"s-1","turns":1,"cost_usd":null,"cost_is_estimate":false,"result":"r"}` + "\n"
	if err := os.MkdirAll(filepath.Dir(history.Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(history.Path(), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, e := runCLI("-r", "s-1", "next"); code != 0 {
		t.Fatal(e)
	}
	if got := addDirArgs(argv(t, record)); len(got) != 0 {
		t.Fatalf("%q", got)
	}
}

func TestAddDirDeletedInheritedDirectoryFailsWithRecovery(t *testing.T) {
	work, record := setup(t, okReply)
	d := mkdirs(t, "a", "b")
	if code, _, e := runCLI("-d", work, "--add-dir", d[0], "first"); code != 0 {
		t.Fatal(e)
	}
	os.RemoveAll(d[0])
	os.Remove(record)
	code, out, e := runCLI("-json", "-r", "s-1", "second")
	if code != 2 {
		t.Fatalf("code=%d out=%q err=%q", code, out, e)
	}
	for _, want := range []string{"cannot resume session s-1", d[0], "--clear-add-dirs", "--add-dir", "offload show s-1"} {
		if !strings.Contains(out+e, want) {
			t.Errorf("missing %q in %q", want, out+e)
		}
	}
	if _, err := os.Stat(record); err == nil {
		t.Fatal("worker must not run")
	}
	// The setup failure keeps the saved set but has no session, so it cannot poison inheritance.
	if r := lastRecord(t); r.Session != "" || !slices.Equal(r.AddDirs, d[:1]) {
		t.Fatalf("failure record: %+v", r)
	}
	if code, _, e := runCLI("-r", "s-1", "--clear-add-dirs", "third"); code != 0 {
		t.Fatal(e)
	}
	os.Remove(record)
	if code, _, e := runCLI("-r", "s-1", "--add-dir", d[1], "fourth"); code != 0 {
		t.Fatal(e)
	}
	if got := addDirArgs(argv(t, record)); !slices.Equal(got, d[1:]) {
		t.Fatalf("%q", got)
	}
}

func TestAddDirKeptWhenDirOverridesOnResume(t *testing.T) {
	work, record := setup(t, okReply)
	d := mkdirs(t, "a", "other")
	if code, _, e := runCLI("-d", work, "--add-dir", d[0], "first"); code != 0 {
		t.Fatal(e)
	}
	t.Chdir(t.TempDir())
	if code, _, e := runCLI("-r", "s-1", "-d", d[1], "second"); code != 0 {
		t.Fatal(e)
	}
	if got := addDirArgs(argv(t, record)); !slices.Equal(got, d[:1]) {
		t.Fatalf("inherited roots must stay absolute and unrebased: %q", got)
	}
	if r := lastRecord(t); r.Dir != d[1] || !slices.Equal(r.AddDirs, d[:1]) {
		t.Fatalf("%+v", r)
	}
	// Inherited set that now equals the new primary directory is omitted.
	if code, _, e := runCLI("-r", "s-1", "-d", d[0], "third"); code != 0 {
		t.Fatal(e)
	}
	if got := addDirArgs(argv(t, record)); len(got) != 0 {
		t.Fatalf("primary must be omitted: %q", got)
	}
}

func TestAddDirSalvageKeepsSameRoots(t *testing.T) {
	work, record := setup(t, okReply)
	d := mkdirs(t, "a")
	t.Setenv("FAKE_PREFIX", `{"type":"system","subtype":"init","session_id":"s-killed"}`)
	t.Setenv("FAKE_SLEEP", "15")
	t.Setenv("FAKE_SALVAGE", "FINDING")
	if code, _, e := runCLI("-json", "-d", work, "--add-dir", d[0], "-t", "4s", "slow"); code != 124 {
		t.Fatal(code, e)
	}
	b, _ := os.ReadFile(record + ".salvage-args")
	if !strings.Contains(string(b), "--add-dir "+d[0]+" ") || !strings.Contains(string(b), "--tools") {
		t.Fatalf("salvage args: %q", b)
	}
}

func TestAddDirCodexPositionsFreshAndResume(t *testing.T) {
	work, record := setupCodex(t, codexReply)
	d := mkdirs(t, "a", "b")
	if code, _, e := runCLI("-p", "codex", "-d", work, "--add-dir", d[0], "--add-dir", d[1], "inspect"); code != 0 {
		t.Fatal(e)
	}
	args := argv(t, record)
	want := []string{"exec", "--dangerously-bypass-approvals-and-sandbox", "--add-dir", d[0], "--add-dir", d[1], "--json", "--skip-git-repo-check", "-m", "test-model", "-"}
	if !slices.Equal(args, want) {
		t.Fatalf("%q want %q", args, want)
	}
	if code, _, e := runCLI("-r", "codex-session", "follow up"); code != 0 {
		t.Fatal(e)
	}
	args = argv(t, record)
	want = []string{"exec", "--dangerously-bypass-approvals-and-sandbox", "--add-dir", d[0], "--add-dir", d[1], "resume", "--json", "--skip-git-repo-check", "-m", "test-model", "codex-session", "-"}
	if !slices.Equal(args, want) {
		t.Fatalf("%q want %q", args, want)
	}
	if stdin, _ := os.ReadFile(record + ".stdin"); !strings.Contains(string(stdin), "Task:\nfollow up") {
		t.Fatalf("stdin: %q", stdin)
	}
}

func TestShowPrintsAddDirs(t *testing.T) {
	work, _ := setup(t, okReply)
	d := mkdirs(t, "a")
	if code, _, e := runCLI("-d", work, "--add-dir", d[0], "first"); code != 0 {
		t.Fatal(e)
	}
	_, out, _ := runCLI("show", "s-1")
	if !strings.Contains(out, "add_dirs=[\""+d[0]+"\"]") {
		t.Fatalf("%q", out)
	}
}
