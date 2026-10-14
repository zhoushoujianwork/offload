package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Record is one line of the run log.
type Record struct {
	TS         string  `json:"ts"`
	Status     string  `json:"status"` // ok | error | timeout
	Profile    string  `json:"profile"`
	Model      string  `json:"model"`
	Dir        string  `json:"dir"`
	Task       string  `json:"task"`
	Session    string  `json:"session"`
	Turns      int     `json:"turns"`
	CostUSD    float64 `json:"cost_usd"`
	DurationMs int64   `json:"duration_ms"`
	Result     string  `json:"result"`
}

func historyPath() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "offload", "runs.jsonl")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "offload", "runs.jsonl")
}

func appendRecord(path string, r Record) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// readRecords returns all records in file order; a missing log is empty, not an error.
func readRecords(path string) ([]Record, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rs []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		var r Record
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			rs = append(rs, r)
		}
	}
	return rs, sc.Err()
}

func bySession(rs []Record, session string) []Record {
	var out []Record
	for _, r := range rs {
		if r.Session == session {
			out = append(out, r)
		}
	}
	return out
}
