package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zhoushoujianwork/offload/internal/history"
	"github.com/zhoushoujianwork/offload/internal/live"
)

const (
	historySchemaVersion = 1
	historyDefaultLimit  = 50
	historyMaxLimit      = 200
	historyMaxQuery      = 4096 // bytes of search text read from stdin
	historyUsage         = "usage: offload history list --json [--limit N] [--offset N] [--search-stdin] | offload history show --json --id <id>"
)

type historyListOutput struct {
	SchemaVersion int                           `json:"schema_version"`
	Total         int                           `json:"total"`
	Offset        int                           `json:"offset"`
	Limit         int                           `json:"limit"`
	HasMore       bool                          `json:"has_more"`
	Conversations []history.ConversationSummary `json:"conversations"`
}

type historyShowOutput struct {
	SchemaVersion int                         `json:"schema_version"`
	Conversation  history.ConversationSummary `json:"conversation"`
	Runs          []history.ConversationRun   `json:"runs"`
	Active        *live.State                 `json:"active"`
}

// cmdHistory serves the structured conversation history: `history list` and `history show`.
// Search text arrives on stdin only, never in argv, where other users could read it.
func cmdHistory(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return fail(stderr, errors.New(historyUsage))
	}
	switch args[0] {
	case "list":
		return historyList(args[1:], stdin, stdout, stderr)
	case "show":
		return historyShow(args[1:], stdout, stderr)
	}
	return fail(stderr, errors.New(historyUsage))
}

func historyFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("offload history "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard) // flag errors can echo the offending value; report our own
	return fs
}

// activeSessions returns the sessions with a running or salvaging worker and those workers' states.
// A lost worker is not busy. A failing live directory only means nothing is known to be active.
func activeSessions() (map[string]bool, map[string]live.State) {
	sessions, states := map[string]bool{}, map[string]live.State{}
	list, err := live.List()
	if err != nil {
		return sessions, states
	}
	for _, s := range list {
		if s.Active() && s.Session != "" {
			sessions[s.Session] = true
			states[s.Session] = s
		}
	}
	return sessions, states
}

func historyList(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := historyFlags("list")
	asJSON := fs.Bool("json", false, "")
	limit := fs.Int("limit", historyDefaultLimit, "")
	offset := fs.Int("offset", 0, "")
	searchStdin := fs.Bool("search-stdin", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fail(stderr, errors.New(historyUsage))
	}
	if !*asJSON {
		return fail(stderr, errors.New("history list needs --json"))
	}
	if *limit < 1 || *limit > historyMaxLimit {
		return fail(stderr, fmt.Errorf("--limit must be between 1 and %d", historyMaxLimit))
	}
	if *offset < 0 {
		return fail(stderr, errors.New("--offset must not be negative"))
	}
	query := ""
	if *searchStdin {
		b, err := io.ReadAll(io.LimitReader(stdin, historyMaxQuery+1))
		if err != nil {
			return fail(stderr, errors.New("read search text from stdin failed"))
		}
		if len(b) > historyMaxQuery {
			return fail(stderr, fmt.Errorf("search text is longer than %d bytes", historyMaxQuery))
		}
		if !utf8.Valid(b) {
			return fail(stderr, errors.New("search text is not valid UTF-8"))
		}
		query = strings.ToLower(strings.TrimSpace(string(b)))
	}

	rs, err := history.Read(history.Path())
	if err != nil {
		return fail(stderr, fmt.Errorf("read run log: %w", err))
	}
	active, _ := activeSessions()
	var matched []history.ConversationSummary
	for _, c := range history.BuildConversations(rs, active, time.Local) {
		if c.Matches(query) {
			matched = append(matched, c.Summary)
		}
	}
	out := historyListOutput{SchemaVersion: historySchemaVersion, Total: len(matched), Offset: *offset, Limit: *limit,
		Conversations: []history.ConversationSummary{}}
	if *offset < len(matched) {
		end := min(*offset+*limit, len(matched))
		out.Conversations = matched[*offset:end]
		out.HasMore = end < len(matched)
	}
	return writeHistoryJSON(stdout, stderr, out)
}

func historyShow(args []string, stdout, stderr io.Writer) int {
	fs := historyFlags("show")
	asJSON := fs.Bool("json", false, "")
	id := fs.String("id", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fail(stderr, errors.New(historyUsage))
	}
	if !*asJSON {
		return fail(stderr, errors.New("history show needs --json"))
	}
	if *id == "" {
		return fail(stderr, errors.New("history show needs --id"))
	}
	rs, err := history.Read(history.Path())
	if err != nil {
		return fail(stderr, fmt.Errorf("read run log: %w", err))
	}
	active, states := activeSessions()
	c, ok := history.FindConversation(rs, *id, active, time.Local)
	if !ok {
		return fail(stderr, errors.New("no conversation with that id"))
	}
	out := historyShowOutput{SchemaVersion: historySchemaVersion, Conversation: c.Summary, Runs: c.Runs}
	if s, ok := states[c.Summary.Session]; ok && c.Summary.Session != "" {
		out.Active = &s
	}
	return writeHistoryJSON(stdout, stderr, out)
}

// writeHistoryJSON encodes to memory first, so a failure never leaves a partial document on stdout.
func writeHistoryJSON(stdout, stderr io.Writer, v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		return fail(stderr, errors.New("encode history JSON failed"))
	}
	if _, err := stdout.Write(append(b, '\n')); err != nil {
		return fail(stderr, errors.New("write history JSON failed"))
	}
	return 0
}
