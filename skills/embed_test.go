package skills

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// embedded lists every skill that embed.go compiles in, keyed by its directory name.
var embedded = map[string][]byte{
	"offload": Offload,
}

// frontMatter splits a skill file into its key/value front matter and the remaining body.
func frontMatter(t *testing.T, data []byte) (map[string]string, string) {
	t.Helper()
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		t.Fatal("file does not start with a --- front matter fence")
	}
	block, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		t.Fatal("front matter is not closed by a --- fence")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(key) == "" {
			t.Fatalf("front matter line %q is not key: value", line)
		}
		fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return fields, body
}

func TestEmbeddedSkillsAreNonEmptyAndParse(t *testing.T) {
	for dir, data := range embedded {
		t.Run(dir, func(t *testing.T) {
			if len(bytes.TrimSpace(data)) == 0 {
				t.Fatal("embedded skill is empty")
			}
			fields, body := frontMatter(t, data)
			if fields["name"] != dir {
				t.Fatalf("front matter name = %q, want %q", fields["name"], dir)
			}
			if fields["description"] == "" {
				t.Fatal("front matter description is empty")
			}
			if strings.TrimSpace(body) == "" {
				t.Fatal("skill body after the front matter is empty")
			}
		})
	}
}

// Every SKILL.md shipped in the source tree must be embedded and match the embedded bytes,
// so a skill added on disk cannot be forgotten in embed.go.
func TestEverySkillFileOnDiskIsEmbedded(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("*", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no SKILL.md files found in the skills directory")
	}
	seen := map[string]bool{}
	for _, file := range files {
		dir := filepath.Base(filepath.Dir(file))
		seen[dir] = true
		data, ok := embedded[dir]
		if !ok {
			t.Errorf("%s exists on disk but is not embedded", file)
			continue
		}
		disk, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(disk, data) {
			t.Errorf("embedded %s differs from %s", dir, file)
		}
	}
	for dir := range embedded {
		if !seen[dir] {
			t.Errorf("embedded skill %s has no SKILL.md on disk", dir)
		}
	}
}
