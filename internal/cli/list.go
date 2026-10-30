package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/zhoushoujianwork/offload/internal/history"
)

func cmdList(args []string, stdout, stderr io.Writer) int {
	if len(args) > 1 {
		return fail(stderr, errors.New("usage: offload ls [n]"))
	}
	n := 10
	if len(args) == 1 {
		v, err := strconv.Atoi(args[0])
		if err != nil || v < 1 {
			return fail(stderr, fmt.Errorf("ls: bad count %q; usage: offload ls [n]", args[0]))
		}
		n = v
	}
	rs, err := history.Read(history.Path())
	if err != nil {
		return fail(stderr, err)
	}
	if len(rs) == 0 {
		fmt.Fprintln(stdout, "No runs yet.")
		return 0
	}
	if len(rs) > n {
		rs = rs[len(rs)-n:]
	}
	for i, r := range rs {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		status := strings.ToUpper(firstNonEmpty(r.Status, "unknown"))
		if r.ExitCode == nil || *r.ExitCode != 0 {
			status += " (exit=" + exitText(r) + ")"
		}
		if class := r.TerminationClass(); class != "" {
			status += " [" + class + "]"
		}
		cost := listCost(r)
		if r.Usage != nil {
			cost += " | " + compactUsage(r)
		}
		profile := firstNonEmpty(r.Profile, "profile unavailable")
		if r.Model != "" && !profileNamesModel(profile, r.Model) {
			profile += " / " + r.Model
		}
		if r.ModelSource != "" {
			profile += " (model_source=" + r.ModelSource + ")"
		}
		project := "project unavailable"
		if r.Dir != "" {
			project = filepath.Base(r.Dir)
		}
		fmt.Fprintf(stdout, "%s | %s | %s | %d turns | %s\n  %s | %s\n  %s\n  Session: %s\n",
			listLine(r.TS), listLine(status), listElapsed(r.ElapsedMs), r.Turns, cost,
			listLine(project), listLine(profile), settingsLine(r.Task, 100), firstNonEmpty(r.Session, "unavailable"))
	}
	fmt.Fprintln(stdout, "\nN/A: see offload show <session> for details. API baseline estimates assume short context/30m cache writes; exclude long-context, regional/tool surcharges and subscription accounting; actual delivery may differ.")
	return 0
}

// profileNamesModel reports whether the profile name already shows the model
// as a whole token, so ls need not repeat it. A bare substring is not enough:
// model "c" must still be shown for profile "codex".
func profileNamesModel(profile, model string) bool {
	p, m := strings.ToLower(profile), strings.ToLower(model)
	boundary := func(s string, i int) bool {
		if i < 0 || i >= len(s) {
			return true
		}
		c := s[i]
		return !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9')
	}
	for i := 0; i+len(m) <= len(p); i++ {
		if p[i:i+len(m)] == m && boundary(p, i-1) && boundary(p, i+len(m)) {
			return true
		}
	}
	return false
}

// listCost marks recorded values as estimates and explains why a missing one is unavailable.
// A recorded zero stays $0.0000; unavailable is never shown as zero.
func listCost(r history.Record) string {
	if r.CostUSD != nil {
		if r.CostBasis == "api_baseline" {
			return costText(r)
		}
		return "est. " + costText(r)
	}
	return "N/A"
}

// Sanitize metadata without shortening profile names or model identifiers.
func listLine(s string) string { return settingsLine(s, int(^uint(0)>>1)) }

func listElapsed(ms *int64) string {
	if ms == nil || *ms < 0 {
		return "elapsed unknown"
	}
	if *ms < 1000 {
		return fmt.Sprintf("%dms", *ms)
	}
	s := *ms / 1000
	if s >= 3600 {
		return fmt.Sprintf("%dh%dm%ds", s/3600, s/60%60, s%60)
	}
	if s >= 60 {
		return fmt.Sprintf("%dm%ds", s/60, s%60)
	}
	return fmt.Sprintf("%ds", s)
}
