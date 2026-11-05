package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
)

const retestCfg = `{"default":"a","keep":{"k":1},"profiles":{"a":{"base_url":"https://a.example","model":"m","token_cmd":"echo a","extra":1},"b":{"base_url":"https://b.example","model":"m","token_cmd":"echo b"}}}`

func setupRetest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("OFFLOAD_PROFILE", "")
	path := config.Path()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(retestCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func stubProbe(t *testing.T, out probeOutcome) *int {
	t.Helper()
	calls := 0
	old := probeProfile
	probeProfile = func(context.Context, string, string) probeOutcome { calls++; return out }
	t.Cleanup(func() { probeProfile = old })
	return &calls
}

func TestRetestRestoresAndUpdatesReason(t *testing.T) {
	path := setupRetest(t)
	if err := config.SetUnavailable(path, "a", "authentication failed: old", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Failure keeps it unavailable and replaces reason and time.
	calls := stubProbe(t, probeOutcome{ErrorKind: "execution", Message: "HTTP 402 insufficient_quota api_key=" + "sk-" + strings.Repeat("z", 20)})
	var out, errb bytes.Buffer
	if code := cmdProfileTest([]string{"a"}, &out, &errb); code != 1 || *calls != 1 {
		t.Fatalf("code=%d calls=%d out=%s err=%s", code, *calls, out.String(), errb.String())
	}
	cfg, _ := config.Load(path)
	u := cfg.Profiles["a"].Unavailable
	if u == nil || !strings.HasPrefix(u.Reason, "quota exhausted") || strings.Contains(u.Reason, "sk-z") || strings.Contains(u.Reason, "authentication failed: old") {
		t.Fatalf("unavailable = %+v", u)
	}
	// A transient failure of an unavailable profile still refreshes the reason and keeps it unavailable.
	stubProbe(t, probeOutcome{ErrorKind: "timeout", Message: "context deadline exceeded"})
	if code := cmdProfileTest([]string{"a"}, &out, &errb); code != 1 {
		t.Fatalf("code=%d", code)
	}
	cfg, _ = config.Load(path)
	if cfg.Profiles["a"].Available() {
		t.Fatal("profile must stay unavailable after a failed test")
	}
	// Success restores it and keeps unknown fields.
	stubProbe(t, probeOutcome{OK: true})
	if code := cmdProfileTest([]string{"a"}, &out, &errb); code != 0 {
		t.Fatalf("code=%d", code)
	}
	cfg, _ = config.Load(path)
	if !cfg.Profiles["a"].Available() {
		t.Fatal("profile should be available")
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), `"keep"`) || !strings.Contains(string(raw), `"extra"`) || !strings.Contains(string(raw), "echo a") {
		t.Fatalf("fields lost: %s", raw)
	}
}

func TestRetestTransientFailureKeepsAvailableProfile(t *testing.T) {
	path := setupRetest(t)
	stubProbe(t, probeOutcome{ErrorKind: "rate_limit", Message: "429 too many requests"})
	var out, errb bytes.Buffer
	if code := cmdProfileTest([]string{"a"}, &out, &errb); code != 1 {
		t.Fatalf("code=%d", code)
	}
	cfg, _ := config.Load(path)
	if !cfg.Profiles["a"].Available() {
		t.Fatal("transient failure must not change state")
	}
}

func TestRetestRejectsConcurrentTest(t *testing.T) {
	path := setupRetest(t)
	lock, err := lockProfileTest(path, "a")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	calls := stubProbe(t, probeOutcome{OK: true})
	var out, errb bytes.Buffer
	if code := cmdProfileTest([]string{"a"}, &out, &errb); code == 0 || *calls != 0 {
		t.Fatalf("second test must be refused: code=%d calls=%d", code, *calls)
	}
}

func TestRecordRunFailureAndListing(t *testing.T) {
	path := setupRetest(t)
	recordRunFailure(path, "a", "rate_limit", "429")
	recordRunFailure(path, "a", "execution", "connection reset by peer")
	if cfg, _ := config.Load(path); !cfg.Profiles["a"].Available() {
		t.Fatal("transient failures must not mark unavailable")
	}
	recordRunFailure(path, "a", "execution", "HTTP 401 Unauthorized")
	snap, err := buildProfileSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	var a profileEntry
	for _, p := range snap.Profiles {
		if p.Name == "a" {
			a = p
		}
	}
	if a.Available || !strings.HasPrefix(a.UnavailableReason, "authentication failed") || a.UnavailableAt == "" {
		t.Fatalf("entry = %+v", a)
	}
	b, _ := json.Marshal(snap)
	if !strings.Contains(string(b), `"unavailable_reason"`) {
		t.Fatal("json lacks reason")
	}
	var out bytes.Buffer
	cmdProfiles(&out, &out)
	if !strings.Contains(out.String(), "unavailable since") {
		t.Fatalf("text listing: %s", out.String())
	}
}
