package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/zhoushoujianwork/offload/internal/config"
)

func cmdAccounts(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || (len(args) == 1 && args[0] == "--json") {
		return writeSnapshotAccounts(stdout, stderr)
	}
	if len(args) != 2 || (args[0] != "add" && args[0] != "import") {
		return fail(stderr, errors.New("usage: offload accounts [--json] | add <name> | import <name> (auth.json on stdin)"))
	}
	home, err := config.CreateManagedAccount(args[1])
	if err != nil {
		return fail(stderr, err)
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(home)
		}
	}()
	if args[0] == "import" {
		data, readErr := io.ReadAll(io.LimitReader(stdin, (1<<20)+1))
		if readErr != nil {
			return fail(stderr, errors.New("could not read authentication"))
		}
		err = config.ImportManagedAuth(home, data)
	} else {
		bin := os.Getenv("OFFLOAD_CODEX")
		if bin == "" {
			bin, err = exec.LookPath("codex")
		}
		if err == nil {
			signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			ctx, cancel := context.WithTimeout(signalCtx, 10*time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "login", "--device-auth")
			cmd.Env = managedCodexEnv(os.Environ(), home)
			cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
			err = cmd.Run()
		}
		if err == nil {
			data, readErr := os.ReadFile(filepath.Join(home, "auth.json"))
			if readErr != nil {
				err = errors.New("Codex did not save account credentials")
			} else {
				err = config.ImportManagedAuth(home, data)
			}
		}
	}
	if err != nil {
		return fail(stderr, errors.New("managed account setup failed; provide a valid Codex OAuth sign-in"))
	}
	success = true
	return writeSnapshotAccounts(stdout, stderr)
}

func managedCodexEnv(base []string, home string) []string {
	env := []string{}
	for _, v := range base {
		key := v
		for i, c := range v {
			if c == '=' {
				key = v[:i]
				break
			}
		}
		switch key {
		case "CODEX_HOME", "OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN":
			continue
		}
		env = append(env, v)
	}
	return append(env, "CODEX_HOME="+home)
}

func writeSnapshotAccounts(stdout, stderr io.Writer) int {
	return writeSnapshot(stdout, stderr, profileSnapshot{SchemaVersion: 1, ManagedAccounts: config.ManagedAccountNames(), Profiles: []profileEntry{}, Warnings: []string{}})
}
