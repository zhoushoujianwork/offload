package execution_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// Compile and exercise a different module. An in-repository test alone would
// not catch accidental internal imports in the public integration surface.
func TestIndependentConsumer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("native fixture executables use Unix shell/process groups")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mod := "module execution-consumer\n\ngo 1.24\n\nrequire github.com/zhoushoujianwork/offload v0.0.0\nreplace github.com/zhoushoujianwork/offload => " + strconv.Quote(filepath.ToSlash(root)) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0600); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{"../../go.sum", "go.sum"}, {"testdata/consumer_test.go.txt", "execution_test.go"}} {
		data, err := os.ReadFile(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, pair[1]), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(t.Context(), "go", "test", "-race", "-mod=mod", "-count=1", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("independent consumer: %v\n%s", err, strings.TrimSpace(string(out)))
	}
}
