package threads

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateKey(t *testing.T) {
	for _, k := range []string{"issue-39", "a/b:c", "任务-1", strings.Repeat("k", MaxKeyBytes)} {
		if err := ValidateKey(k); err != nil {
			t.Errorf("%q: %v", k, err)
		}
	}
	for _, k := range []string{"", "has space", "tab\t", "new\nline", "nul\x00", "\xff", strings.Repeat("k", MaxKeyBytes+1)} {
		if ValidateKey(k) == nil {
			t.Errorf("%q accepted", k)
		}
	}
}

func TestSaveLoadSurvivesNewHandle(t *testing.T) {
	dir := t.TempDir()
	h, err := Acquire(dir, "k", "/work")
	if err != nil {
		t.Fatal(err)
	}
	if e, err := h.Load(); e != nil || err != nil {
		t.Fatalf("fresh thread: %+v %v", e, err)
	}
	if err := h.Save(nil, "claude", "cheap", "s-1", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	first, _ := h.Load()
	if err := h.Save(first, "claude", "cheap", "s-2", time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}
	h.Release()

	h2, err := Acquire(dir, "k", "/work")
	if err != nil {
		t.Fatal(err)
	}
	defer h2.Release()
	e, err := h2.Load()
	if err != nil || e == nil || e.Session != "s-2" || e.Backend != "claude" || e.Profile != "cheap" || e.Created != first.Created || e.Updated == e.Created {
		t.Fatalf("%+v %v", e, err)
	}
	if fi, err := os.Stat(h2.Path()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", fi, err)
	}
}

func TestScopedByKeyAndDirectory(t *testing.T) {
	dir := t.TempDir()
	a, _ := Acquire(dir, "k", "/work/a")
	defer a.Release()
	if err := a.Save(nil, "claude", "p", "s-a", time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{"k", "/work/b"}, {"other", "/work/a"}} {
		h, err := Acquire(dir, c[0], c[1])
		if err != nil {
			t.Fatalf("%v: %v", c, err)
		}
		if e, err := h.Load(); e != nil || err != nil {
			t.Errorf("%v shares a mapping: %+v %v", c, e, err)
		}
		h.Release()
	}
}

func TestAcquireIsExclusive(t *testing.T) {
	dir := t.TempDir()
	h, err := Acquire(dir, "k", "/w")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Acquire(dir, "k", "/w")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("second claim: %v", err)
	}
	if other, err := Acquire(dir, "j", "/w"); err != nil {
		t.Fatalf("another key blocked: %v", err)
	} else {
		other.Release()
	}
	h.Release()
	again, err := Acquire(dir, "k", "/w")
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again.Release()
}

func TestCorruptMappingIsReportedAndIsolated(t *testing.T) {
	dir := t.TempDir()
	good, _ := Acquire(dir, "good", "/w")
	if err := good.Save(nil, "claude", "p", "s-good", time.Now()); err != nil {
		t.Fatal(err)
	}
	good.Release()

	for name, content := range map[string]string{
		"garbage":   "{not json",
		"empty":     "",
		"no-id":     `{"version":1,"key":"bad","dir":"/w","backend":"claude","profile":"p"}`,
		"wrong-key": `{"version":1,"key":"other","dir":"/w","backend":"claude","profile":"p","session":"s"}`,
		"version":   `{"version":9,"key":"bad","dir":"/w","backend":"claude","profile":"p","session":"s"}`,
	} {
		bad, err := Acquire(dir, "bad", "/w")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(bad.Path(), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if e, err := bad.Load(); e != nil || !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %+v %v", name, e, err)
		}
		if b, _ := os.ReadFile(bad.Path()); string(b) != content {
			t.Errorf("%s: Load modified the file", name)
		}
		bad.Release()
	}
	g, _ := Acquire(dir, "good", "/w")
	defer g.Release()
	if e, err := g.Load(); err != nil || e == nil || e.Session != "s-good" {
		t.Fatalf("other thread affected: %+v %v", e, err)
	}
}

func TestSaveLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	h, _ := Acquire(dir, "k", "/w")
	defer h.Release()
	for i := 0; i < 3; i++ {
		if err := h.Save(nil, "claude", "p", "s", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := filepath.Glob(filepath.Join(dir, ".thread-*"))
	if len(m) != 0 {
		t.Fatalf("temp files: %v", m)
	}
}
