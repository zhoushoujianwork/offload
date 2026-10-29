package history

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeCaller(t *testing.T) {
	for in, want := range map[string]string{"agh": "agh", "ClawFlow": "clawflow", "A.b_c-9": "a.b_c-9", "0x": "0x", strings.Repeat("a", 64): strings.Repeat("a", 64)} {
		if got, err := NormalizeCaller(in); err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", " ", "a b", " agh", "agh ", "a\tb", "a\nb", "-a", ".a", "_a", "a/b", "a:b", "ägh", "aghK", "İ", strings.Repeat("a", 65)} {
		if got, err := NormalizeCaller(in); err == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
}

func TestLegacyRecordsReadAsUnspecifiedWithoutRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runs.jsonl")
	legacy := `{"ts":"t","status":"ok","exit_code":0,"profile":"p","model":"m","dir":"d","task":"x","session":"s1","cost_usd":null,"result":"r"}` + "\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	rs, err := Read(path)
	if err != nil || len(rs) != 1 || rs[0].Caller != CallerUnspecified {
		t.Fatalf("%+v %v", rs, err)
	}
	if b, _ := os.ReadFile(path); string(b) != legacy {
		t.Fatalf("source rewritten: %s", b)
	}
	if err := Append(path, Record{Session: "s2", Caller: "agh"}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), legacy) || !strings.Contains(string(b), `"caller":"agh"`) {
		t.Fatalf("append altered older lines: %s", b)
	}
}
