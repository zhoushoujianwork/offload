// Package live publishes the state of a running worker, so a supervisor can see it work, or
// notice it stuck, long before the run ends. One small file per offload process, rewritten
// every couple of seconds. It is advisory: nothing depends on it to run or to report.
package live

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/zhoushoujianwork/offload/internal/history"
)

const (
	Running   = "running"
	Salvaging = "salvaging" // stopped; asking the worker to report what it already read
	Done      = "done"
	Lost      = "lost" // derived: a running state nobody has refreshed (offload was killed)
)

// StaleAfter and ExpireAfter are variables only so tests can shorten them.
var (
	StaleAfter  = 30 * time.Second
	ExpireAfter = 10 * time.Minute
)

type State struct {
	PID         int       `json:"pid"`
	Phase       string    `json:"phase"`
	Status      string    `json:"status,omitempty"` // set once Done: ok | error | timeout
	ExitCode    int       `json:"exit_code,omitempty"`
	Dir         string    `json:"dir"`
	Task        string    `json:"task"`
	Profile     string    `json:"profile"`
	Model       string    `json:"model"`
	Session     string    `json:"session,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	LastEventAt time.Time `json:"last_event_at"`
	ToolCalls   int       `json:"tool_calls"`
	Denials     int       `json:"permission_denials"`
	LastTool    string    `json:"last_tool,omitempty"`
}

// Active reports whether the worker is still doing something.
func (s State) Active() bool { return s.Phase == Running || s.Phase == Salvaging }

// Dir sits next to the run log.
func Dir() string { return filepath.Join(filepath.Dir(history.Path()), "live") }

// Publish atomically writes the state for its process.
func Publish(s State) error {
	s.UpdatedAt = time.Now()
	if err := os.MkdirAll(Dir(), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	final := filepath.Join(Dir(), strconv.Itoa(s.PID)+".json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// List returns the known workers oldest first. Expired files are deleted, and an active state
// that stopped being refreshed is reported as Lost.
func List() ([]State, error) {
	entries, err := os.ReadDir(Dir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []State
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(Dir(), e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var s State
		if json.Unmarshal(b, &s) != nil {
			continue
		}
		age := time.Since(s.UpdatedAt)
		if age > ExpireAfter {
			os.Remove(path)
			continue
		}
		if s.Active() && age > StaleAfter {
			s.Phase = Lost
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out, nil
}
