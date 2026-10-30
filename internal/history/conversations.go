package history

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ConversationsSchemaVersion is the schema_version of the history list and show documents.
const ConversationsSchemaVersion = 1

// TitleRunes bounds a conversation title.
const TitleRunes = 100

// Prefixes of ids that are not worker session ids.
const (
	orphanIDPrefix = "run:" // conversation id of a record with no session
	runIDPrefix    = "rec:" // id of one run
)

// ConversationSummary describes one conversation without its reports. Usage and CostUSD sum only the
// runs whose own figures are trusted (see runUsage and runCost); UsageRuns and CostRuns say how many of
// RunCount that is, so a nil or partial total is never mistaken for a complete one.
type ConversationSummary struct {
	ID        string      `json:"id"`
	Session   string      `json:"session"`
	Title     string      `json:"title"`
	Dir       string      `json:"dir"`
	AddDirs   []string    `json:"add_dirs,omitempty"` // the latest run's effective set
	Profile   string      `json:"profile"`
	Backend   string      `json:"backend"`
	Model     string      `json:"model"`
	StartedAt string      `json:"started_at"`
	UpdatedAt string      `json:"updated_at"`
	RunCount  int         `json:"run_count"`
	Status    string      `json:"status"`
	Active    bool        `json:"active"`
	Usage     *TokenUsage `json:"usage"`
	UsageRuns int         `json:"usage_runs"`
	CostUSD   *float64    `json:"cost_usd"`
	CostRuns  int         `json:"cost_runs"`

	ResumeAvailable bool `json:"resume_available"`
}

// ConversationRun is one stored run with its full task and report. Usage and CostUSD are the run's
// own trusted figures, nil when unavailable or excluded (salvaged, legacy or cumulative).
type ConversationRun struct {
	ID       string      `json:"id"`
	TS       string      `json:"ts"`
	Status   string      `json:"status"`
	ExitCode *int        `json:"exit_code"`
	Task     string      `json:"task"`
	Result   string      `json:"result"`
	Error    string      `json:"error"`
	Dir      string      `json:"dir"`
	AddDirs  []string    `json:"add_dirs,omitempty"`
	Profile  string      `json:"profile"`
	Backend  string      `json:"backend"`
	Model    string      `json:"model"`
	Session  string      `json:"session"`
	Usage    *TokenUsage `json:"usage"`
	CostUSD  *float64    `json:"cost_usd"`
	Salvaged bool        `json:"salvaged"`
	Elapsed  *int64      `json:"elapsed_ms"`
	Turns    int         `json:"turns"`

	RunID           string `json:"run_id,omitempty"`
	ResumedFrom     string `json:"resumed_from,omitempty"`
	ResumeAvailable bool   `json:"resume_available"`
}

// Conversation is a summary plus its runs in chronological order.
type Conversation struct {
	Summary ConversationSummary
	Runs    []ConversationRun
}

type convRun struct {
	r     Record
	idx   int
	at    time.Time
	valid bool
}

// BuildConversations groups records into conversations, newest first. Records sharing a non-empty
// session form one conversation whose id is the session. A record without a session is its own
// conversation with the opaque id "run:<hash>", so failures that never reached a session stay
// visible. Nothing is dropped: a record with an unreadable timestamp is ordered next to its
// neighbours in the log. activeSessions holds the sessions a worker is running or salvaging.
// The result is never nil.
func BuildConversations(rs []Record, activeSessions map[string]bool, loc *time.Location) []Conversation {
	if loc == nil {
		loc = time.Local
	}
	groups := map[string][]convRun{}
	var order []string // group keys in first-record order
	for i, r := range rs {
		key := r.Session
		if key == "" {
			key = orphanIDPrefix + recordHash(r, i)
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		at, ok := ParseTS(r.TS, loc)
		groups[key] = append(groups[key], convRun{r: r, idx: i, at: at, valid: ok})
	}

	out := make([]Conversation, 0, len(order))
	type sortKey struct {
		latest  time.Time
		lastIdx int
	}
	keys := make([]sortKey, 0, len(order))
	for _, key := range order {
		runs := groups[key]
		fillMissingTimes(runs)
		sort.SliceStable(runs, func(i, j int) bool {
			if !runs[i].at.Equal(runs[j].at) {
				return runs[i].at.Before(runs[j].at)
			}
			return runs[i].idx < runs[j].idx
		})
		last := runs[len(runs)-1]
		keys = append(keys, sortKey{latest: last.at, lastIdx: maxIdx(runs)})
		out = append(out, summarize(key, runs, activeSessions))
	}
	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool {
		ka, kb := keys[idx[a]], keys[idx[b]]
		if !ka.latest.Equal(kb.latest) {
			return ka.latest.After(kb.latest)
		}
		return ka.lastIdx > kb.lastIdx
	})
	sorted := make([]Conversation, len(out))
	for i, j := range idx {
		sorted[i] = out[j]
	}
	return sorted
}

// FindConversation returns the conversation with exactly this id: a session id, or an opaque
// "run:<hash>" id of a record without a session.
func FindConversation(rs []Record, id string, activeSessions map[string]bool, loc *time.Location) (Conversation, bool) {
	if id == "" {
		return Conversation{}, false
	}
	for _, c := range BuildConversations(rs, activeSessions, loc) {
		if c.Summary.ID == id {
			return c, true
		}
	}
	return Conversation{}, false
}

// Matches reports whether the lower-cased query occurs, ignoring case, in the task, result, profile,
// model, dir or session of any run. An empty query matches everything.
func (c Conversation) Matches(lowerQuery string) bool {
	if lowerQuery == "" {
		return true
	}
	for _, r := range c.Runs {
		for _, f := range [...]string{r.Task, r.Result, r.Profile, r.Model, r.Dir, r.Session} {
			if strings.Contains(strings.ToLower(f), lowerQuery) {
				return true
			}
		}
	}
	return false
}

// fillMissingTimes gives a run with an unreadable timestamp the time of the closest earlier readable
// run in log order (or the first readable one, if none precedes it). With no readable time at all
// every run keeps the zero time and the log order decides.
func fillMissingTimes(runs []convRun) {
	var carry time.Time
	have := false
	for i := range runs {
		if runs[i].valid {
			carry, have = runs[i].at, true
		} else if have {
			runs[i].at = carry
		}
	}
	for i := range runs {
		if runs[i].valid {
			for j := 0; j < i; j++ {
				if !runs[j].valid {
					runs[j].at = runs[i].at
				}
			}
			return
		}
	}
}

func maxIdx(runs []convRun) int {
	m := runs[0].idx
	for _, r := range runs[1:] {
		if r.idx > m {
			m = r.idx
		}
	}
	return m
}

func summarize(id string, runs []convRun, activeSessions map[string]bool) Conversation {
	var acc statsAcc
	dtos := make([]ConversationRun, len(runs))
	for i, e := range runs {
		r := e.r
		acc.add(r, r.ElapsedMs != nil && *r.ElapsedMs >= 0)
		dtos[i] = ConversationRun{ID: runIDPrefix + recordHash(r, e.idx), TS: r.TS, Status: r.Status, ExitCode: r.ExitCode,
			RunID: r.RunID, ResumedFrom: r.ResumedFrom, ResumeAvailable: r.HasResumeSettings() && !activeSessions[r.Session],
			Task: r.Task, Result: r.Result, Error: r.Error, Dir: r.Dir, AddDirs: slices.Clone(r.AddDirs), Profile: r.Profile, Backend: r.Backend,
			Model: r.Model, Session: r.Session, Usage: runUsage(r), CostUSD: runCost(r), Salvaged: r.Salvaged,
			Elapsed: r.ElapsedMs, Turns: r.Turns}
	}
	t := acc.totals()
	first, last := runs[0].r, runs[len(runs)-1].r
	session := last.Session
	s := ConversationSummary{ID: id, Session: session, Title: title(first.Task),
		ResumeAvailable: last.HasResumeSettings() && !activeSessions[session],
		Dir:             latest(runs, func(r Record) string { return r.Dir }),
		AddDirs:         slices.Clone(last.AddDirs),
		Profile:         latest(runs, func(r Record) string { return r.Profile }),
		Backend:         latest(runs, func(r Record) string { return r.Backend }),
		Model:           latest(runs, func(r Record) string { return r.Model }),
		StartedAt:       first.TS, UpdatedAt: last.TS, RunCount: len(runs), Status: last.Status,
		Active: session != "" && activeSessions[session],
		Usage:  t.Usage, UsageRuns: t.UsageRuns, CostUSD: t.CostUSD, CostRuns: t.CostRuns}
	return Conversation{Summary: s, Runs: dtos}
}

// latest returns the field of the most recent run that recorded one.
func latest(runs []convRun, get func(Record) string) string {
	for i := len(runs) - 1; i >= 0; i-- {
		if v := get(runs[i].r); v != "" {
			return v
		}
	}
	return ""
}

// title is the first non-blank line of the task, cut to TitleRunes runes.
func title(task string) string {
	line := ""
	for _, l := range strings.Split(task, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			line = l
			break
		}
	}
	if utf8.RuneCountInString(line) <= TitleRunes {
		return line
	}
	return string([]rune(line)[:TitleRunes-1]) + "…"
}

// recordHash identifies a record by its content and position in the log. It is stable while the log
// only grows, and distinguishes identical records at different positions.
func recordHash(r Record, index int) string {
	h := sha256.New()
	for _, f := range []string{r.TS, r.Status, r.Profile, r.Model, r.Dir, r.Session, r.Task, strconv.Itoa(index)} {
		h.Write([]byte(strconv.Itoa(len(f))))
		h.Write([]byte{':'})
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))[:20]
}
