//go:build unix

package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveTokenContextRunsTokenCmd(t *testing.T) {
	tok, err := (Profile{TokenCmd: "echo tok-1"}).ResolveTokenContext(context.Background())
	if err != nil || tok != "tok-1" {
		t.Fatalf("tok=%q err=%v", tok, err)
	}
	tok, err = (Profile{TokenCmd: "echo tok-2"}).ResolveToken()
	if err != nil || tok != "tok-2" {
		t.Fatalf("legacy tok=%q err=%v", tok, err)
	}
}

func TestResolveTokenContextPreCancelledDoesNotRun(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (Profile{TokenCmd: "echo x > '" + marker + "'; echo tok"}).ResolveTokenContext(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("token_cmd ran")
	}
}

// Cancelling while token_cmd runs returns within a bound, and the command's later effect never happens.
func TestResolveTokenContextCancelStopsTokenCmd(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "late")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	start := time.Now()
	_, err := (Profile{TokenCmd: "(sleep 1; echo late > '" + marker + "') & wait; echo tok"}).ResolveTokenContext(ctx)
	if d := time.Since(start); d > 800*time.Millisecond {
		t.Fatalf("returned after %s", d)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("descendant of token_cmd survived cancellation")
	}
}
