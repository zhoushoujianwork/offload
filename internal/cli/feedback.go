package cli

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
)

// feedbackURL is the project homepage. Users reach Issues from there and sign
// in through GitHub themselves; offload does not authenticate or file anything.
const feedbackURL = "https://github.com/zhoushoujianwork/offload"

// openURL launches the platform's default browser. Tests replace it so that no
// real browser or external process is started.
var openURL = launchBrowser

// browserCommand returns the default-browser launcher for goos.
func browserCommand(goos, url string) (string, []string, error) {
	switch goos {
	case "darwin":
		return "open", []string{url}, nil
	case "linux", "freebsd", "openbsd", "netbsd", "dragonfly":
		return "xdg-open", []string{url}, nil
	case "windows":
		return "rundll32", []string{"url.dll,FileProtocolHandler", url}, nil
	}
	return "", nil, fmt.Errorf("no default browser launcher for %s", goos)
}

func launchBrowser(url string) error {
	name, args, err := browserCommand(runtime.GOOS, url)
	if err != nil {
		return err
	}
	if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		if len(out) > 0 {
			return fmt.Errorf("%s: %w: %s", name, err, trimOutput(out))
		}
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

func trimOutput(b []byte) string {
	const max = 200
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	if len(s) > max {
		s = s[:max] + "..."
	}
	return s
}

// cmdFeedback prints the project homepage URL, then tries to open it in the
// default browser. The URL is printed first so it stays usable when the launch
// fails or no browser exists (headless hosts).
func cmdFeedback(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		return fail(stderr, errors.New("usage: offload feedback"))
	}
	fmt.Fprintln(stdout, feedbackURL)
	if err := openURL(feedbackURL); err != nil {
		fmt.Fprintf(stderr, "offload: could not open a browser: %v\noffload: open this URL manually: %s\n", err, feedbackURL)
		return 1
	}
	return 0
}
