package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zhoushoujianwork/offload/internal/config"
	"github.com/zhoushoujianwork/offload/internal/live"
	"github.com/zhoushoujianwork/offload/internal/schedule"
)

// lifecycleArgs accepts positional names before optional flags, like profile test.
func lifecycleArgs(args []string, allowAccount bool) (name string, account, jsonOut bool, err error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", false, false, errors.New("expected a profile or account name")
	}
	name = args[0]
	seen := map[string]bool{}
	for _, arg := range args[1:] {
		if seen[arg] || (arg != "--json" && !(allowAccount && arg == "--account")) {
			return "", false, false, errors.New("unexpected argument: " + arg)
		}
		seen[arg] = true
		account = account || arg == "--account"
		jsonOut = jsonOut || arg == "--json"
	}
	return name, account, jsonOut, nil
}

func profileMatches(cfg *config.Config, canonical, ref string) bool {
	if ref == canonical {
		return true
	}
	for _, alias := range cfg.Profiles[canonical].Aliases {
		if ref == alias {
			return true
		}
	}
	return false
}

func activeLifecycleStates() ([]live.State, error) {
	states, unreadable, err := live.ListChecked()
	if err != nil {
		return nil, err
	}
	if unreadable != 0 {
		return nil, errors.New("cannot verify active workers: unreadable live state")
	}
	return states, nil
}

func checkAccountRemoval(cfg *config.Config, account, excluding string, states []live.State) error {
	if _, err := config.RemovableAccountHome(account); err != nil {
		return err
	}
	for name, p := range cfg.Profiles {
		if name != excluding && p.CodexAccount == account {
			return fmt.Errorf("account %q is referenced by profile %q", account, name)
		}
	}
	home := filepath.Join(config.Dir(), "codex-accounts", account)
	for _, st := range states {
		if !st.Active() && !st.Unresolved {
			continue
		}
		used := st.CodexAccount == account || (st.CodexHome != "" && filepath.Clean(st.CodexHome) == home)
		for name, p := range cfg.Profiles {
			used = used || (p.CodexAccount == account && profileMatches(cfg, name, st.Profile))
		}
		if used {
			return fmt.Errorf("account %q is used by an active or queued worker", account)
		}
	}
	return nil
}

func cmdProfileLifecycle(args []string, stdout, stderr io.Writer) int {
	name, account, jsonOut, err := lifecycleArgs(args[1:], args[0] == "delete")
	if err != nil {
		return fail(stderr, err)
	}
	cfg, err := config.Load(config.Path())
	if err != nil {
		return fail(stderr, err)
	}
	name, p, err := cfg.Resolve(name)
	if err != nil {
		return fail(stderr, err)
	}
	if args[0] != "delete" {
		err = config.SetProfileDisabled(config.Path(), name, args[0] == "disable")
	} else {
		if name == cfg.Default {
			return fail(stderr, errors.New("set another default first"))
		}
		states, stateErr := activeLifecycleStates()
		if stateErr != nil {
			return fail(stderr, stateErr)
		}
		for _, st := range states {
			if (st.Active() || st.Unresolved) && profileMatches(cfg, name, st.Profile) {
				return fail(stderr, fmt.Errorf("profile %q is used by an active or queued worker", name))
			}
		}
		tasks, warnings, listErr := schedule.NewStore("").List()
		if listErr != nil {
			return fail(stderr, listErr)
		}
		if len(warnings) > 0 {
			return fail(stderr, errors.New("cannot verify schedule references: unreadable scheduled task"))
		}
		var ids []string
		for _, task := range tasks {
			if profileMatches(cfg, name, task.Profile) {
				ids = append(ids, task.ID)
			}
		}
		if len(ids) > 0 {
			sort.Strings(ids)
			return fail(stderr, fmt.Errorf("profile %q is referenced by schedules: %s", name, strings.Join(ids, ", ")))
		}
		if account {
			if p.CodexAccount == "" {
				return fail(stderr, errors.New("profile has no managed Codex account"))
			}
			if err := checkAccountRemoval(cfg, p.CodexAccount, name, states); err != nil {
				return fail(stderr, err)
			}
		}
		err = config.DeleteProfile(config.Path(), name)
		if err == nil && account {
			err = config.RemoveManagedAccount(p.CodexAccount)
		}
	}
	if err != nil {
		return fail(stderr, err)
	}
	if !jsonOut {
		msg := map[string]string{"disable": "disabled", "enable": "enabled", "delete": "deleted"}[args[0]] + " profile " + name
		if account {
			msg += " and managed account " + p.CodexAccount
		}
		fmt.Fprintln(stdout, msg)
		return 0
	}
	snap, err := buildProfileSnapshot(config.Path())
	if err != nil {
		return fail(stderr, err)
	}
	return writeSnapshot(stdout, stderr, snap)
}

func cmdAccountRemove(args []string, stdout, stderr io.Writer) int {
	name, _, jsonOut, err := lifecycleArgs(args, false)
	if err != nil {
		return fail(stderr, err)
	}
	cfg, err := config.Load(config.Path())
	if _, statErr := os.Stat(config.Path()); errors.Is(statErr, os.ErrNotExist) {
		cfg, err = &config.Config{}, nil
	}
	if err != nil {
		return fail(stderr, err)
	}
	states, err := activeLifecycleStates()
	if err != nil {
		return fail(stderr, err)
	}
	if err := checkAccountRemoval(cfg, name, "", states); err != nil {
		return fail(stderr, err)
	}
	if err := config.RemoveManagedAccount(name); err != nil {
		return fail(stderr, err)
	}
	if !jsonOut {
		fmt.Fprintln(stdout, "removed managed account "+name)
		return 0
	}
	return writeSnapshotAccounts(stdout, stderr)
}
