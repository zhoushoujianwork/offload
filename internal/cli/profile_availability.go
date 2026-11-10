package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/slots"
	"github.com/zhoushoujianwork/offload/pkg/execution"
)

const profileTestTask = "Reply with the single word OK."

var errProfileTestBusy = errors.New("a test for this profile is already running")

// probeOutcome is the result of one minimal real model call.
type probeOutcome struct {
	OK        bool
	ErrorKind string
	Message   string
}

// probeProfile is replaced in tests. The default makes one real, minimal model call.
var probeProfile = func(ctx context.Context, path, name string) probeOutcome {
	cfg, err := execution.LoadConfig(path, name)
	if err != nil {
		return probeOutcome{ErrorKind: "configuration", Message: err.Error()}
	}
	client, err := execution.New(cfg)
	if err != nil {
		return probeOutcome{ErrorKind: "configuration", Message: err.Error()}
	}
	dir, err := os.MkdirTemp("", "offload-profile-test-")
	if err != nil {
		return probeOutcome{ErrorKind: "configuration", Message: err.Error()}
	}
	defer os.RemoveAll(dir)
	timeout := 2 * time.Minute
	res, err := client.Run(ctx, execution.Request{Task: profileTestTask, Directory: dir, Timeout: &timeout})
	if err != nil && res.Status == "" {
		return probeOutcome{ErrorKind: res.ErrorKind, Message: err.Error()}
	}
	if res.Status == "ok" {
		return probeOutcome{OK: true}
	}
	return probeOutcome{ErrorKind: res.ErrorKind, Message: res.Error}
}

// recordRunFailure marks a profile unavailable after a run failed for a reason that does not
// recover by itself. Transient failures and successful runs leave the state unchanged.
func recordRunFailure(path, profile, errorKind, message string) {
	if profile == "" {
		return
	}
	if permanent, category := config.ClassifyFailure(errorKind, message); permanent {
		_ = config.SetUnavailable(path, profile, config.FailureReason(category, message), time.Now())
	}
}

// retestProfile runs one probe and updates the stored state. Success restores the profile; a
// failure keeps an unavailable profile unavailable with the new reason and time, and marks an
// available one unavailable only for unrecoverable errors.
func retestProfile(ctx context.Context, path, name string) (probeOutcome, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return probeOutcome{}, errConfigUnreadable
	}
	canonical, p, err := cfg.Resolve(name)
	if err != nil {
		return probeOutcome{}, errors.New("unknown profile")
	}
	lock, err := lockProfileTest(path, canonical)
	if err != nil {
		return probeOutcome{}, err
	}
	defer lock.Release()
	out := probeProfile(ctx, path, canonical)
	switch {
	case out.ErrorKind == "canceled" || ctx.Err() != nil:
		return probeOutcome{ErrorKind: "canceled", Message: "profile test canceled"}, nil
	case out.OK:
		return out, config.MarkAvailable(path, canonical)
	}
	permanent, category := config.ClassifyFailure(out.ErrorKind, out.Message)
	if permanent || !p.Available() {
		if category == "" {
			category = "test failed"
		}
		return out, config.SetUnavailable(path, canonical, config.FailureReason(category, out.Message), time.Now())
	}
	return out, nil
}

func lockProfileTest(path, name string) (*slots.SessionLock, error) {
	sum := sha256.Sum256([]byte(path + "\x00" + name))
	dir := filepath.Join(slots.Dir(), "profile-tests")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lock, busy, err := slots.LockFile(filepath.Join(dir, hex.EncodeToString(sum[:8])+".lock"))
	if err != nil {
		return nil, err
	}
	if busy {
		return nil, errProfileTestBusy
	}
	return lock, nil
}

// cmdProfileTest handles `offload profile test <name>`.
func cmdProfileTest(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return fail(stderr, errors.New("usage: offload profile test <profile>"))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	out, err := retestProfile(ctx, config.Path(), args[0])
	if err != nil {
		return fail(stderr, err)
	}
	if out.OK {
		fmt.Fprintf(stdout, "profile %s is available\n", args[0])
		return 0
	}
	fmt.Fprintf(stdout, "profile test failed: %s\n", config.SanitizeReason(out.Message))
	return 1
}
