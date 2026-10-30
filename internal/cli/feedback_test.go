package cli

import (
	"errors"
	"strings"
	"testing"
)

func stubOpenURL(t *testing.T, fn func(string) error) {
	t.Helper()
	old := openURL
	openURL = fn
	t.Cleanup(func() { openURL = old })
}

func TestBrowserCommandSelectsPlatformLauncher(t *testing.T) {
	const u = "https://example.test/x"
	cases := []struct {
		goos string
		name string
		args []string
	}{
		{"darwin", "open", []string{u}},
		{"linux", "xdg-open", []string{u}},
		{"windows", "rundll32", []string{"url.dll,FileProtocolHandler", u}},
	}
	for _, c := range cases {
		name, args, err := browserCommand(c.goos, u)
		if err != nil || name != c.name || strings.Join(args, "\x00") != strings.Join(c.args, "\x00") {
			t.Errorf("%s: got %q %q %v", c.goos, name, args, err)
		}
	}
	if _, _, err := browserCommand("plan9", u); err == nil {
		t.Error("unsupported platform must return an error")
	}
}

func TestFeedbackOpensHomepageAndPrintsURL(t *testing.T) {
	var opened []string
	stubOpenURL(t, func(u string) error { opened = append(opened, u); return nil })
	code, out, errOut := runCLI("feedback")
	if code != 0 || errOut != "" {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
	if strings.TrimSpace(out) != "https://github.com/zhoushoujianwork/offload" {
		t.Fatalf("stdout = %q", out)
	}
	if len(opened) != 1 || opened[0] != feedbackURL {
		t.Fatalf("opened = %v", opened)
	}
}

func TestFeedbackLaunchFailureStillPrintsURL(t *testing.T) {
	stubOpenURL(t, func(string) error { return errors.New("xdg-open: executable file not found") })
	code, out, errOut := runCLI("feedback")
	if code == 0 {
		t.Fatal("launch failure must exit non-zero")
	}
	if !strings.Contains(out, feedbackURL) {
		t.Fatalf("stdout must contain URL: %q", out)
	}
	if !strings.Contains(errOut, "could not open a browser") || !strings.Contains(errOut, "executable file not found") || !strings.Contains(errOut, feedbackURL) {
		t.Fatalf("stderr = %q", errOut)
	}
}

func TestFeedbackRejectsArgsWithoutLaunching(t *testing.T) {
	stubOpenURL(t, func(string) error { t.Fatal("must not launch"); return nil })
	if code, _, errOut := runCLI("feedback", "extra"); code != 2 || !strings.Contains(errOut, "usage: offload feedback") {
		t.Fatalf("code=%d stderr=%q", code, errOut)
	}
}

func TestUsageListsFeedback(t *testing.T) {
	if !strings.Contains(usage, "offload feedback") {
		t.Fatal("usage must list feedback")
	}
}
