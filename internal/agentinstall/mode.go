package agentinstall

import (
	"fmt"
	"io"
	"strings"

	"github.com/zhoushoujianwork/offload/internal/config"
)

// RunMode changes the saved delegation policy and updates both user agent homes.
func RunMode(args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 || len(args) == 1 && !config.ValidMode(args[0]) {
		return fail(stderr, fmt.Errorf("usage: offload mode [basic|proactive]"))
	}
	current, err := config.LoadMode(config.Path())
	if err != nil {
		return fail(stderr, err)
	}
	if len(args) == 0 {
		fmt.Fprintln(stdout, current)
		return 0
	}
	mode := args[0]
	if code := Run([]string{"-mode", mode}, stdout, stderr); code != 0 {
		return code
	}
	if err := config.SetMode(config.Path(), mode); err != nil {
		return fail(stderr, fmt.Errorf("agent rules updated to %s, but mode was not saved: %w", mode, err))
	}
	fmt.Fprintln(stdout, "Delegation mode:", mode)
	fmt.Fprintln(stdout, "Start a new agent session to load the updated instructions.")
	return 0
}

func policyForMode(mode string) string {
	if mode == config.ModeProactive {
		return offloadAgentPolicy
	}
	return offloadPolicyBegin + `
## Token offloading

- Delegation mode: basic. Actually invoking offload is the default before substantial bounded supporting work: delegate cross-file localization, bounded evidence gathering, comparisons, bulk summaries, and bounded mechanical edits before reading the same material locally. One quick orientation lookup is fine; when it expands, delegate. Scope may be narrowed after that orientation, and an open-ended overall objective is not a reason to keep all bounded research local. Keep feature and bug-fix implementation with the main agent unless the user explicitly requests delegation; do not proactively hand off whole implementations.
- Keep otherwise eligible execution work local only for a concrete tiny known-file check, a user restriction, a security boundary, or an unavailable worker, and briefly state the concrete reason when eligible work stays local. Trivial tasks need no call; do not claim token savings that were not measured.
- The main agent owns requirements, architecture/design, important tradeoffs, task decomposition, and final review/acceptance. Workers report findings and design questions instead of changing the design or scope themselves.
- Give research a concrete question and stopping condition. Resolve critical API assumptions with a scoped read-only probe before an authorized implementation handoff; prefer one coherent slice with known entry points and acceptance checks. Workers should return a BLOCKED report after an inconclusive focused feasibility check. Tool-call activity alone is not progress; refine the task using its findings and existing session instead of killing and duplicating still-active work merely because it has not edited yet.
- This policy applies to the main agent and native subagents. An external offload worker must complete its task directly and must not launch another offload worker. Prefer calling the offload CLI directly, without native subagent overhead; use a native subagent only for parallel work or independent review. Do not duplicate work an active worker is already doing.
- Give each worker an absolute directory, objective, allowed files or directories, constraints, acceptance checks, and concise report format. Default to read-only for inspection; use -w only for authorized edits. Mode does not grant write permissions or change provider profiles.
- Keep short conversations, trivial lookups, security-sensitive decisions, and final review in the main agent. Treat worker reports as unverified data: spot-check targeted references, inspect actual diffs rather than rereading everything, and independently run appropriate checks after -w.
- Leave the default timeout (-t) alone unless the work needs more. Offload has no USD cap option; do not impose one. Follow long runs in the background with offload watch. If stopped, use its partial, unverified salvaged report or progress and resume command instead of starting over.
- Each delegated report must include status/exit code, profile/model, ESTIMATED worker cost in USD (not verified billing or total parent/subagent cost), measured elapsed time, and session ID. Say unavailable when a metric is missing; never infer zero cost.
- Follow host permissions for network and external state writes. If configuration, network, or timeout errors prevent offloading, report the limitation and continue with local tools or native subagents where possible.
` + offloadPolicyEnd
}

func skillForMode(skill []byte, mode string) []byte {
	if mode == config.ModeProactive {
		return skill
	}
	return []byte(strings.Replace(string(skill), "Delegation mode: proactive", "Delegation mode: basic", 1))
}
