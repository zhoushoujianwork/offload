package worker

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/zhoushoujianwork/offload/internal/config"
)

const managedPrompt = `You are a worker delegated a task by a supervising agent. Complete the assigned task autonomously within its authorized scope.
Task-authorized tests, git commits, git pushes, and PR operations through clawflow are permitted. Do not perform unsolicited merges, force pushes, destructive operations, or unrelated changes. Do not launch offload or recursively delegate.
Nobody can approve permission prompts; work within the configured permissions. Preserve all supplied operator outcome markers and reporting requirements.
Finish with a concise report of changes, exact file:line references, checks and unresolved issues.
`

var errEventsIO = errors.New("events I/O failed")
var callerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

type configError struct {
	kind string
	err  error
}

func (e *configError) Error() string { return e.err.Error() }
func ErrorKind(err error) string {
	if errors.Is(err, config.ErrDisabled) {
		return "globally_disabled"
	}
	var e *configError
	if errors.As(err, &e) {
		return e.kind
	}
	return "configuration"
}
func policyName(s string) string {
	if s == "" {
		return "default"
	}
	return s
}
func NewRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Values of the on-unsupported-tools option.
const (
	UnsupportedToolsFail   = "fail"
	UnsupportedToolsIgnore = "ignore"
)

// requestedToolFlags names the tool restriction flags the caller passed.
func requestedToolFlags(o *Opts) []string {
	var flags []string
	if o.ToolsDisabled {
		flags = append(flags, "-tools-disabled")
	}
	if o.AllowedTools != "" {
		flags = append(flags, "-allowed-tools")
	}
	if o.DisallowedTools != "" {
		flags = append(flags, "-disallowed-tools")
	}
	return flags
}

// CheckToolRestrictions validates the tool restriction flags against the selected backend. It
// needs no worker slot, so callers run it during argument handling. The result is true when the
// restriction cannot be enforced and the caller chose -on-unsupported-tools ignore: the run then
// proceeds without it and must record tool_restriction: not_enforced.
func CheckToolRestrictions(o *Opts) (notEnforced bool, err error) {
	switch o.OnUnsupportedTools {
	case "", UnsupportedToolsFail, UnsupportedToolsIgnore:
	default:
		return false, fmt.Errorf("invalid on-unsupported-tools %q (want fail or ignore)", o.OnUnsupportedTools)
	}
	if o.ToolsDisabled && o.AllowedTools != "" {
		return false, errors.New("tools-disabled conflicts with allowed-tools")
	}
	flags := requestedToolFlags(o)
	if len(flags) == 0 || o.Profile.SupportsToolRestrictions() {
		return false, nil
	}
	if o.OnUnsupportedTools == UnsupportedToolsIgnore {
		return true, nil
	}
	return false, &configError{"unsupported_tools", fmt.Errorf("%s cannot be enforced: profile %q uses the %s backend, which does not support tool restrictions. "+
		"Remove %s, choose a Claude profile with -p, or pass -on-unsupported-tools ignore to run without the restriction (the result and history then record tool_restriction: not_enforced)",
		strings.Join(flags, ", "), o.ProfileName, o.Profile.BackendName(), strings.Join(flags, ", "))}
}

// Validate checks supervisor constraints before any provider is started.
func Validate(o *Opts) error {
	o.ExecutionPolicy = policyName(o.ExecutionPolicy)
	if o.ExecutionPolicy != "default" && o.ExecutionPolicy != "managed-development" {
		return errors.New("invalid execution-policy")
	}
	managed := o.ExecutionPolicy == "managed-development"
	if managed && !o.Write {
		return errors.New("managed-development requires -w")
	}
	if o.CallerRunID != "" && !callerPattern.MatchString(o.CallerRunID) {
		return errors.New("invalid caller-run-id")
	}
	if o.CodexSandbox != "" && (!managed || !o.Write || (o.CodexSandbox != "workspace-write" && o.CodexSandbox != "danger-full-access")) {
		return errors.New("codex-sandbox requires managed-development and -w; expected workspace-write or danger-full-access")
	}
	if o.CodexSandbox != "" && o.Profile.BackendName() != "codex" {
		return errors.New("codex-sandbox requires Codex backend")
	}
	notEnforced, err := CheckToolRestrictions(o)
	if err != nil {
		return err
	}
	o.toolsNotEnforced = notEnforced
	for _, list := range []string{o.AllowedTools, o.DisallowedTools} {
		if list != "" {
			for _, v := range strings.Split(list, ",") {
				if strings.TrimSpace(v) == "" {
					return errors.New("tool list contains empty entry")
				}
			}
		}
	}
	if o.InstructionsFile != "" {
		b, err := readInstructions(o.InstructionsFile)
		if err != nil {
			return err
		}
		o.Instructions = b
	}
	if managed {
		o.NoSalvage = true
	}
	if managed && o.Profile.BackendName() == "codex" && o.CodexSandbox != "danger-full-access" {
		o.GitRoots = nil
		for _, flag := range []string{"--absolute-git-dir", "--git-common-dir"} {
			cmd := exec.Command("git", "-C", o.Dir, "rev-parse", flag)
			b, err := cmd.Output()
			if err != nil {
				var exit *exec.ExitError
				if errors.As(err, &exit) && exit.ExitCode() == 128 {
					// Outside a repository Git has no administrative directories to add.
					if _, statErr := os.Stat(filepath.Join(o.Dir, ".git")); os.IsNotExist(statErr) {
						continue
					}
				}
				return fmt.Errorf("resolve git sandbox roots: %w", err)
			}
			path := strings.TrimSpace(string(b))
			if !filepath.IsAbs(path) {
				path = filepath.Join(o.Dir, path)
			}
			root, err := filepath.EvalSymlinks(path)
			if err != nil {
				return fmt.Errorf("resolve git root: %w", err)
			}
			o.GitRoots = append(o.GitRoots, root)
		}
	}
	return nil
}
func readInstructions(path string) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("instructions-file must be absolute")
	}
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > 256<<10 {
		return nil, errors.New("instructions-file must be non-symlink regular file <=256KiB")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	actual, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !actual.Mode().IsRegular() || !os.SameFile(st, actual) || actual.Size() > 256<<10 {
		return nil, errors.New("instructions-file changed or invalid")
	}
	b, err := io.ReadAll(io.LimitReader(f, (256<<10)+1))
	if len(b) > 256<<10 {
		return nil, errors.New("instructions-file exceeds 256KiB")
	}
	return b, err
}
